package authoritative

import (
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/castlemilk/dns/internal/zone"
	"github.com/miekg/dns"
)

const DefaultMaxUDPSize = 1232

const maxGlueRecords = 32

type Server struct {
	logger   *slog.Logger
	maxUDP   uint16
	queries  atomic.Uint64
	snapshot atomic.Pointer[snapshot]
}

type snapshot struct {
	zones       map[string]*compiledZone
	zoneCount   uint32
	recordCount uint32
}

// CompiledSnapshot is a fully validated, immutable set of authoritative zones.
// It can be prepared before publishing so readers observe either the old or the
// new snapshot, never a partially compiled state.
type CompiledSnapshot struct {
	value *snapshot
}

type compiledZone struct {
	name   string
	owners map[string]map[uint16][]dns.RR
	exists map[string]struct{}
	cuts   map[string][]dns.RR
	soa    []dns.RR
}

func New(logger *slog.Logger, maxUDP uint16) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if maxUDP < dns.MinMsgSize {
		maxUDP = DefaultMaxUDPSize
	}
	server := &Server{logger: logger, maxUDP: maxUDP}
	server.snapshot.Store(&snapshot{zones: make(map[string]*compiledZone)})
	return server
}

func (s *Server) Replace(values []zone.Zone) error {
	next, err := Compile(values)
	if err != nil {
		return err
	}
	s.ReplaceCompiled(next)
	return nil
}

// Compile validates and compiles zones without changing a live Server.
func Compile(values []zone.Zone) (*CompiledSnapshot, error) {
	next, err := compileSnapshot(values)
	if err != nil {
		return nil, fmt.Errorf("compile authoritative snapshot: %w", err)
	}
	return &CompiledSnapshot{value: next}, nil
}

// ReplaceCompiled atomically publishes a snapshot previously returned by
// Compile. A nil snapshot is ignored defensively.
func (s *Server) ReplaceCompiled(next *CompiledSnapshot) {
	if next == nil || next.value == nil {
		return
	}
	s.snapshot.Store(next.value)
}

func (s *Server) QueryCount() uint64 {
	return s.queries.Load()
}

func (s *Server) Counts() (uint32, uint32) {
	current := s.snapshot.Load()
	return current.zoneCount, current.recordCount
}

func (s *Server) ServeDNS(writer dns.ResponseWriter, request *dns.Msg) {
	s.queries.Add(1)
	response := new(dns.Msg)
	response.SetReply(request)
	response.Authoritative = false
	response.RecursionAvailable = false
	response.Compress = true
	requestOPT := request.IsEdns0()

	switch {
	case request.Opcode != dns.OpcodeQuery:
		response.Rcode = dns.RcodeNotImplemented
	case len(request.Question) != 1:
		response.Rcode = dns.RcodeFormatError
	case requestOPT != nil && requestOPT.Version() != 0:
		response.SetRcode(request, dns.RcodeBadVers)
	default:
		s.answer(response, request.Question[0])
	}

	s.addEDNS(request, response)
	if strings.HasPrefix(writer.LocalAddr().Network(), "udp") {
		response.Truncate(s.responseSize(request))
	}
	if err := writer.WriteMsg(response); err != nil {
		s.logger.Debug("write DNS response", "error", err)
	}
}

func (s *Server) answer(response *dns.Msg, question dns.Question) {
	if question.Qclass != dns.ClassINET && question.Qclass != dns.ClassANY {
		response.Rcode = dns.RcodeRefused
		return
	}
	qname := canonical(question.Name)
	value := s.snapshot.Load()
	compiled := value.findZone(qname)
	if compiled == nil {
		response.Rcode = dns.RcodeRefused
		return
	}

	if cut := compiled.findCut(qname); cut != "" {
		response.Rcode = dns.RcodeSuccess
		response.Ns = cloneRRs(compiled.cuts[cut], "")
		response.Extra = append(response.Extra, compiled.glue(response.Ns)...)
		return
	}

	response.Authoritative = true
	if owner, ok := compiled.owners[qname]; ok {
		if question.Qtype == dns.TypeANY {
			response.Answer = minimalANY(qname)
			return
		}
		if rrset := owner[question.Qtype]; len(rrset) > 0 {
			response.Answer = cloneRRs(rrset, "")
			return
		}
		if cname := owner[dns.TypeCNAME]; len(cname) > 0 {
			response.Answer = cloneRRs(cname, "")
			response.Answer = append(response.Answer, compiled.localCNAMEAnswer(cname, question.Qtype)...)
			return
		}
		response.Ns = compiled.negativeSOA()
		return
	}

	// Empty non-terminals exist even though they have no RRsets of their own.
	// Their response is NODATA; a wildcard must not synthesize through them.
	if _, exists := compiled.exists[qname]; exists {
		response.Ns = compiled.negativeSOA()
		return
	}

	wildcard := compiled.wildcard(qname)
	if wildcard != "" {
		owner := compiled.owners[wildcard]
		if question.Qtype == dns.TypeANY {
			response.Answer = minimalANY(qname)
			return
		}
		if rrset := owner[question.Qtype]; len(rrset) > 0 {
			response.Answer = cloneRRs(rrset, qname)
			return
		}
		if cname := owner[dns.TypeCNAME]; len(cname) > 0 {
			response.Answer = cloneRRs(cname, qname)
			response.Answer = append(response.Answer, compiled.localCNAMEAnswer(cname, question.Qtype)...)
			return
		}
		response.Ns = compiled.negativeSOA()
		return
	}

	response.Rcode = dns.RcodeNameError
	response.Ns = compiled.negativeSOA()
}

func minimalANY(owner string) []dns.RR {
	return []dns.RR{&dns.HINFO{
		Hdr: dns.RR_Header{Name: owner, Rrtype: dns.TypeHINFO, Class: dns.ClassINET, Ttl: 60},
		Cpu: "RFC8482",
		Os:  "",
	}}
}

func (s *Server) addEDNS(request, response *dns.Msg) {
	requestOPT := request.IsEdns0()
	if requestOPT == nil {
		return
	}
	responseOPT := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}}
	responseOPT.SetUDPSize(uint16(s.responseSize(request)))
	if requestOPT.Do() {
		responseOPT.SetDo()
	}
	response.Extra = append(response.Extra, responseOPT)
}

func (s *Server) responseSize(request *dns.Msg) int {
	requestOPT := request.IsEdns0()
	if requestOPT == nil {
		return dns.MinMsgSize
	}
	size := requestOPT.UDPSize()
	if size < dns.MinMsgSize {
		size = dns.MinMsgSize
	}
	if size > s.maxUDP {
		size = s.maxUDP
	}
	return int(size)
}

func compileSnapshot(values []zone.Zone) (*snapshot, error) {
	result := &snapshot{
		zones:     make(map[string]*compiledZone, len(values)),
		zoneCount: uint32(len(values)),
	}
	for _, value := range values {
		compiled, err := compileZone(value)
		if err != nil {
			return nil, fmt.Errorf("zone %q: %w", value.Name, err)
		}
		if _, exists := result.zones[compiled.name]; exists {
			return nil, fmt.Errorf("duplicate zone %q", value.Name)
		}
		result.zones[compiled.name] = compiled
		result.recordCount += uint32(len(value.Records))
	}
	return result, nil
}

func compileZone(value zone.Zone) (*compiledZone, error) {
	compiled := &compiledZone{
		name:   canonical(value.Name),
		owners: make(map[string]map[uint16][]dns.RR),
		exists: make(map[string]struct{}),
		cuts:   make(map[string][]dns.RR),
	}
	for _, record := range value.Records {
		rr, err := zone.Compile(value.Name, record)
		if err != nil {
			return nil, fmt.Errorf("record %q: %w", record.ID, err)
		}
		owner := canonical(rr.Header().Name)
		rr.Header().Name = owner
		if compiled.owners[owner] == nil {
			compiled.owners[owner] = make(map[uint16][]dns.RR)
		}
		compiled.owners[owner][rr.Header().Rrtype] = append(compiled.owners[owner][rr.Header().Rrtype], rr)
		compiled.addExistence(owner)
	}

	apex, ok := compiled.owners[compiled.name]
	if !ok || len(apex[dns.TypeSOA]) != 1 {
		return nil, fmt.Errorf("zone apex must contain exactly one SOA record")
	}
	compiled.soa = apex[dns.TypeSOA]
	for owner, rrsets := range compiled.owners {
		if owner != compiled.name && len(rrsets[dns.TypeNS]) > 0 {
			compiled.cuts[owner] = rrsets[dns.TypeNS]
		}
	}
	return compiled, nil
}

func (s *snapshot) findZone(qname string) *compiledZone {
	for current := qname; current != "."; current = parent(current) {
		if value := s.zones[current]; value != nil {
			return value
		}
	}
	return nil
}

func (z *compiledZone) addExistence(owner string) {
	for current := owner; dns.IsSubDomain(z.name, current); current = parent(current) {
		z.exists[current] = struct{}{}
		if current == z.name {
			return
		}
	}
}

func (z *compiledZone) findCut(qname string) string {
	for current := qname; current != z.name && dns.IsSubDomain(z.name, current); current = parent(current) {
		if _, ok := z.cuts[current]; ok {
			return current
		}
	}
	return ""
}

func (z *compiledZone) wildcard(qname string) string {
	for current := parent(qname); dns.IsSubDomain(z.name, current); current = parent(current) {
		if _, exists := z.exists[current]; exists {
			candidate := "*." + current
			if _, ok := z.owners[candidate]; ok {
				return candidate
			}
			return ""
		}
		if current == z.name {
			return ""
		}
	}
	return ""
}

func (z *compiledZone) localCNAMEAnswer(cname []dns.RR, qtype uint16) []dns.RR {
	if qtype == dns.TypeCNAME || len(cname) == 0 {
		return nil
	}
	value, ok := cname[0].(*dns.CNAME)
	if !ok {
		return nil
	}
	target := canonical(value.Target)
	if !dns.IsSubDomain(z.name, target) {
		return nil
	}
	if z.findCut(target) != "" {
		return nil
	}
	return cloneRRs(z.owners[target][qtype], "")
}

func (z *compiledZone) glue(nameservers []dns.RR) []dns.RR {
	result := make([]dns.RR, 0)
	for _, rr := range nameservers {
		ns, ok := rr.(*dns.NS)
		if !ok {
			continue
		}
		target := canonical(ns.Ns)
		if !dns.IsSubDomain(z.name, target) {
			continue
		}
		for _, recordType := range []uint16{dns.TypeA, dns.TypeAAAA} {
			for _, address := range z.owners[target][recordType] {
				result = append(result, dns.Copy(address))
				if len(result) == maxGlueRecords {
					return result
				}
			}
		}
	}
	return result
}

func (z *compiledZone) negativeSOA() []dns.RR {
	result := cloneRRs(z.soa, "")
	if len(result) == 0 {
		return result
	}
	if soa, ok := result[0].(*dns.SOA); ok && soa.Minttl < soa.Hdr.Ttl {
		soa.Hdr.Ttl = soa.Minttl
	}
	return result
}

func cloneRRs(values []dns.RR, owner string) []dns.RR {
	result := make([]dns.RR, 0, len(values))
	for _, value := range values {
		cloned := dns.Copy(value)
		if owner != "" {
			cloned.Header().Name = owner
		}
		result = append(result, cloned)
	}
	return result
}

func canonical(name string) string {
	return strings.ToLower(dns.Fqdn(name))
}

func parent(name string) string {
	name = canonical(name)
	if name == "." {
		return "."
	}
	separator := strings.IndexByte(name, '.')
	if separator < 0 || separator == len(name)-1 {
		return "."
	}
	return name[separator+1:]
}

var _ dns.Handler = (*Server)(nil)
