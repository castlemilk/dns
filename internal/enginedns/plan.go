package enginedns

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/zone"
)

// Plan operation names, as reported to the UI in RecordChange.op.
const (
	ChangeAdopt   = "adopt"
	ChangeAlign   = "align"
	ChangeBlocked = "blocked"
	ChangeCreate  = "create"
	ChangeKeep    = "keep"
	ChangeRemove  = "remove"
	ChangeReplace = "replace"
)

// Conflict is an existing record that stands in the way of the desired set. A
// conflict is only ever removed by an explicit RPC with
// replace_conflicting_records; every worker path reports it instead.
type Conflict struct {
	RecordID string
	Name     string
	Type     zone.RecordType
	TTL      uint32
	Value    string
	// Source names who owns the conflicting record: "" for the user, or the
	// other engine. Only a user record is ever replaced automatically.
	Source string
	Reason string
}

// Replaceable reports whether an explicit replace may delete this record. A
// record another engine owns is never deleted: its reconciler would re-create
// it on its next pass and the two would flap the RRset between them.
func (c Conflict) Replaceable() bool {
	return c.Source == zone.SourceUser
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s %s at %s %s", c.Type, c.Value, c.Name, c.Reason)
}

// RecordChange describes one planned or applied change in the vocabulary the
// UI renders. It mirrors platform.v1.RecordChange.
type RecordChange struct {
	Op            string
	Name          string
	Type          string
	Value         string
	TTL           uint32
	RecordID      string
	PreviousValue string
	Why           string
}

// Proto converts one change onto the wire.
func (c RecordChange) Proto() *platformv1.RecordChange {
	return &platformv1.RecordChange{
		Op:            c.Op,
		Name:          c.Name,
		Type:          c.Type,
		Value:         c.Value,
		Ttl:           c.TTL,
		RecordId:      c.RecordID,
		PreviousValue: c.PreviousValue,
		Why:           c.Why,
	}
}

// ZonePlan is the difference between a zone and one engine's desired records.
type ZonePlan struct {
	// Source is the engine the plan belongs to ("hosting" or "mail").
	Source string
	// Adopt turns an equal user record into an engine record, keeping its id.
	Adopt []Op
	// Align brings other members of a touched RRset onto its TTL. Without it a
	// second gateway address, or SPF beside a TTL-0 verification TXT, would be
	// refused by the RRset rules.
	Align []Op
	// Create adds the records that do not exist yet.
	Create []Op
	// Keep lists the engine records that are already correct. These operations
	// are never applied.
	Keep []Op
	// Remove drops engine records of this source that are no longer desired.
	Remove []Op
	// Conflicts are user records that must go before the desired set fits.
	Conflicts []Conflict
	// Blocked are the operations above that the store will refuse while a
	// conflict stands, each naming the conflict in the way. They are still
	// listed in Adopt/Align/Create — the plan describes the whole desired set —
	// but WorkerOps and ApplyOps leave them out, so one record nobody can write
	// never stops the records beside it from being published.
	Blocked []Blocked
	// Changes describes everything above in UI vocabulary, conflicts included.
	Changes []RecordChange
}

// Blocked is one operation a standing conflict makes unappliable. The only
// rule that produces one is CNAME exclusivity: a create beside a user CNAME,
// or a CNAME beside anything, is refused by the store however many other
// operations share the set — and [zone.Store.ApplyRecordSet] is atomic, so
// leaving it in would roll back every unrelated record with it.
type Blocked struct {
	// Op is the operation that cannot be applied.
	Op Op
	// ConflictIDs are the records standing in its way. Deleting all of them —
	// what ApplyOps(true) does for replaceable conflicts — unblocks the
	// operation.
	ConflictIDs []string
	// Reason is the operator-facing sentence naming what is in the way.
	Reason string
}

// InSync reports whether the zone already carries the desired set with nothing
// standing in its way.
func (p ZonePlan) InSync() bool {
	return len(p.Create) == 0 && len(p.Remove) == 0 && len(p.Conflicts) == 0
}

// WorkerOps are the operations a background reconciler may apply: it adopts,
// aligns, creates and removes its own records, and never deletes a user record.
// Operations a conflict blocks are left out, so the rest of the set still
// reaches the zone.
func (p ZonePlan) WorkerOps() []Op {
	return p.applicableOps(nil)
}

// applicableOps is WorkerOps with the conflicts in removed treated as already
// deleted, which is how ApplyOps(true) recovers the operations a replaceable
// conflict was blocking.
func (p ZonePlan) applicableOps(removed map[string]struct{}) []Op {
	blocked := make(map[string]struct{}, len(p.Blocked))
	for _, item := range p.Blocked {
		if stillBlocked(item, removed) {
			blocked[opKey(item.Op)] = struct{}{}
		}
	}
	ops := make([]Op, 0, len(p.Align)+len(p.Adopt)+len(p.Create)+len(p.Remove))
	for _, group := range [][]Op{p.Align, p.Adopt, p.Create, p.Remove} {
		for _, op := range group {
			if _, stopped := blocked[opKey(op)]; stopped {
				continue
			}
			ops = append(ops, op)
		}
	}
	return ops
}

func stillBlocked(item Blocked, removed map[string]struct{}) bool {
	for _, recordID := range item.ConflictIDs {
		if _, gone := removed[recordID]; !gone {
			return true
		}
	}
	return len(item.ConflictIDs) == 0
}

// opKey identifies an operation across the plan's slices. A create has no
// record id yet, so it is keyed by the record it would write — which is unique
// within a plan, because the desired set is de-duplicated by value.
func opKey(op Op) string {
	if op.RecordID != "" {
		return "id:" + op.RecordID
	}
	return "new:" + op.Record.Name + "/" + string(op.Record.Type) + "/" + op.Record.Value
}

func changeKey(change RecordChange) string {
	if change.RecordID != "" {
		return "id:" + change.RecordID
	}
	return "new:" + change.Name + "/" + change.Type + "/" + change.Value
}

// ApplyOps are the operations an explicit RPC applies. With replaceConflicts
// the conflicting user records are deleted first; the alignment of a record
// that is about to be deleted is dropped with it.
func (p ZonePlan) ApplyOps(replaceConflicts bool) []Op {
	if !replaceConflicts || len(p.Conflicts) == 0 {
		return p.WorkerOps()
	}
	replaced := make(map[string]struct{}, len(p.Conflicts))
	ops := make([]Op, 0, len(p.Conflicts)+len(p.Align)+len(p.Adopt)+len(p.Create)+len(p.Remove))
	for _, conflict := range p.Conflicts {
		if !conflict.Replaceable() {
			continue
		}
		replaced[conflict.RecordID] = struct{}{}
		// Replaceable() is only true for a user record, and the store refuses
		// to delete one unless the operation says so — this is the single
		// place in the process that may. The conflict was read off the stored
		// record, so it can describe it back to the store, which every delete
		// has to.
		ops = append(ops, Op{
			Kind:     OpDelete,
			RecordID: conflict.RecordID,
			Record: zone.Record{
				ID:     conflict.RecordID,
				Name:   conflict.Name,
				Type:   conflict.Type,
				TTL:    conflict.TTL,
				Value:  conflict.Value,
				Source: conflict.Source,
			},
			ReplaceUserRecord: true,
		})
	}
	if len(replaced) == 0 {
		return p.WorkerOps()
	}
	for _, op := range p.applicableOps(replaced) {
		if op.Kind != OpCreate {
			if _, dropped := replaced[op.RecordID]; dropped {
				continue
			}
		}
		ops = append(ops, op)
	}
	return ops
}

// Problems renders the conflicts as the DNSProblems strings the cards show.
func (p ZonePlan) Problems() []string {
	problems := make([]string, 0, len(p.Conflicts))
	for _, conflict := range p.Conflicts {
		problems = append(problems, conflict.String())
	}
	return problems
}

// ChangesProto converts the whole change list onto the wire.
func (p ZonePlan) ChangesProto() []*platformv1.RecordChange {
	changes := make([]*platformv1.RecordChange, 0, len(p.Changes))
	for _, change := range p.Changes {
		changes = append(changes, change.Proto())
	}
	return changes
}

type recordKey struct {
	name string
	kind zone.RecordType
}

// Plan computes the difference between the zone and one engine's desired
// records. desired carries Name, Type, TTL and Value; Source is set from the
// source argument, and a zero TTL falls back to the type's default.
//
// Matching is by value, not by id: a user record that already carries the
// desired value is adopted in place (its id and its RRset TTL survive), so a
// hand-made "point the website here" A record becomes the engine's record
// instead of a duplicate. A/AAAA compare by address, targets by FQDN and TXT by
// wire text, so the one-string and the parenthesised split form of a long DKIM
// key are the same record.
//
// The result type is [ZonePlan], not `Plan`: spec2 §3.4's signature
// `Plan(...) (Plan, error)` cannot be written in Go, because a package declares
// each name once.
func Plan(z zone.Zone, source string, desired []zone.Record) (ZonePlan, error) {
	if source == SourceUser || !ValidSource(source) {
		return ZonePlan{}, fmt.Errorf("plan zone %q: %q is not an engine source", z.Name, source)
	}

	wanted, err := normalizeDesired(z.Name, source, desired)
	if err != nil {
		return ZonePlan{}, err
	}

	plan := ZonePlan{Source: source}
	existingByKey := map[recordKey][]zone.Record{}
	existingByName := map[string][]zone.Record{}
	for _, record := range z.Records {
		if record.Managed {
			continue
		}
		key := recordKey{name: record.Name, kind: record.Type}
		existingByKey[key] = append(existingByKey[key], record)
		existingByName[record.Name] = append(existingByName[record.Name], record)
	}

	conflicts := map[string]Conflict{}
	matched := map[string]struct{}{}

	addConflict := func(record zone.Record, reason string) {
		if _, seen := conflicts[record.ID]; seen {
			return
		}
		conflicts[record.ID] = Conflict{
			RecordID: record.ID,
			Name:     record.Name,
			Type:     record.Type,
			TTL:      record.TTL,
			Value:    record.Value,
			Source:   record.Source,
			Reason:   reason,
		}
	}

	// Cross-type CNAME rules first: they decide which records survive, and
	// therefore which TTL the RRsets below inherit.
	planCnameConflicts(wanted, existingByName, addConflict)

	for _, key := range desiredKeys(wanted) {
		group := wanted[key]
		members := existingByKey[key]
		ttl := effectiveTTL(key, members, group)

		for _, want := range group {
			engine, ok := findValue(members, want, source)
			switch {
			case ok && engine.TTL == ttl:
				matched[engine.ID] = struct{}{}
				plan.Keep = append(plan.Keep, Op{Kind: OpKeep, RecordID: engine.ID, Record: engine})
				plan.Changes = append(plan.Changes, change(ChangeKeep, engine, engine.ID, "", "this record is already published"))
				continue
			case ok:
				matched[engine.ID] = struct{}{}
				aligned := engine
				aligned.TTL = ttl
				plan.Align = append(plan.Align, Op{Kind: OpUpdate, RecordID: engine.ID, Record: aligned})
				plan.Changes = append(plan.Changes, change(ChangeAlign, aligned, engine.ID, "", ttlWhy(engine.TTL, ttl)))
				continue
			}
			if user, ok := findValue(members, want, SourceUser); ok {
				matched[user.ID] = struct{}{}
				adopted := user
				adopted.Source = source
				adopted.TTL = ttl
				plan.Adopt = append(plan.Adopt, Op{Kind: OpUpdate, RecordID: user.ID, Record: adopted})
				plan.Changes = append(plan.Changes, change(ChangeAdopt, adopted, user.ID, "",
					fmt.Sprintf("an existing record already carries this value; %s takes it over", strings.ToLower(SourceLabel(source)))))
				continue
			}
			created := want
			created.TTL = ttl
			plan.Create = append(plan.Create, Op{Kind: OpCreate, Record: created})
			plan.Changes = append(plan.Changes, change(ChangeCreate, created, "", "", ""))
		}

		// Everything left in this RRset either conflicts, belongs to another
		// engine, or simply shares the name and only needs the RRset TTL.
		for _, member := range members {
			if _, ok := matched[member.ID]; ok {
				continue
			}
			switch {
			case member.Source == source:
				plan.Remove = append(plan.Remove, Op{Kind: OpDelete, RecordID: member.ID, Record: member})
				plan.Changes = append(plan.Changes, change(ChangeRemove, member, member.ID, "", "this record is no longer part of the set"))
				continue
			case member.Source != SourceUser:
				addConflict(member, fmt.Sprintf("is written by the %s engine", strings.ToLower(SourceLabel(member.Source))))
			case conflictingUserRecord(key, member, group):
				addConflict(member, conflictReason(key.kind))
			}
			if member.TTL != ttl {
				aligned := member
				aligned.TTL = ttl
				plan.Align = append(plan.Align, Op{Kind: OpUpdate, RecordID: member.ID, Record: aligned})
				plan.Changes = append(plan.Changes, change(ChangeAlign, aligned, member.ID, "", ttlWhy(member.TTL, ttl)))
			}
		}
	}

	// Engine records outside the desired set, anywhere in the zone: a www CNAME
	// left behind after skip_www, a DKIM selector Stalwart rotated away.
	for _, record := range z.Records {
		if record.Managed || record.Source != source {
			continue
		}
		if _, ok := matched[record.ID]; ok {
			continue
		}
		if slices.ContainsFunc(plan.Remove, func(op Op) bool { return op.RecordID == record.ID }) {
			continue
		}
		plan.Remove = append(plan.Remove, Op{Kind: OpDelete, RecordID: record.ID, Record: record})
		plan.Changes = append(plan.Changes, change(ChangeRemove, record, record.ID, "", "this record is no longer part of the set"))
	}

	plan.Conflicts = sortedConflicts(conflicts)
	plan.markBlocked()
	for _, conflict := range plan.Conflicts {
		plan.Changes = append(plan.Changes, RecordChange{
			Op:            ChangeReplace,
			Name:          conflict.Name,
			Type:          string(conflict.Type),
			TTL:           conflict.TTL,
			RecordID:      conflict.RecordID,
			PreviousValue: conflict.Value,
			Why:           conflict.String(),
		})
	}
	return plan, nil
}

// markBlocked records the operations the store cannot apply while a conflict
// stands, and rewrites their change rows so the card names the one record that
// could not be written instead of promising it.
//
// The only rule that can do this is CNAME exclusivity (zone.validateRecordSets):
// a CNAME cannot share an owner with any other record, in either direction.
// Every other clash the planner reports — a user A beside the engine's A, a
// second SPF TXT — is a conflict the operator may choose to replace, but it
// does not stop the write. A conflict is never deleted by a worker, so an
// operation this catches is guaranteed to fail; leaving it in the set would
// roll back every unrelated record with it, because ApplyRecordSet is atomic.
func (p *ZonePlan) markBlocked() {
	byName := map[string][]Conflict{}
	for _, conflict := range p.Conflicts {
		byName[conflict.Name] = append(byName[conflict.Name], conflict)
	}
	if len(byName) == 0 {
		return
	}
	for _, group := range [][]Op{p.Align, p.Adopt, p.Create} {
		for _, op := range group {
			blocked, ok := blockedBy(op, byName[op.Record.Name])
			if !ok {
				continue
			}
			p.Blocked = append(p.Blocked, blocked)
		}
	}
	if len(p.Blocked) == 0 {
		return
	}
	reasons := make(map[string]string, len(p.Blocked))
	for _, item := range p.Blocked {
		reasons[opKey(item.Op)] = item.Reason
	}
	for index, change := range p.Changes {
		switch change.Op {
		case ChangeAdopt, ChangeAlign, ChangeCreate:
		default:
			continue
		}
		if reason, stopped := reasons[changeKey(change)]; stopped {
			p.Changes[index].Op = ChangeBlocked
			p.Changes[index].Why = reason
		}
	}
}

func blockedBy(op Op, conflicts []Conflict) (Blocked, bool) {
	var ids []string
	var reason string
	for _, conflict := range conflicts {
		if conflict.RecordID == op.RecordID {
			continue
		}
		if conflict.Type != zone.TypeCNAME && op.Record.Type != zone.TypeCNAME {
			continue
		}
		ids = append(ids, conflict.RecordID)
		if reason == "" {
			reason = fmt.Sprintf(
				"not written: the %s %s at %s has to go first, because a CNAME cannot share a name with any other record",
				conflict.Type, conflict.Value, conflict.Name)
		}
	}
	if len(ids) == 0 {
		return Blocked{}, false
	}
	return Blocked{Op: op, ConflictIDs: ids, Reason: reason}, true
}

func normalizeDesired(zoneName, source string, desired []zone.Record) (map[recordKey][]zone.Record, error) {
	wanted := map[recordKey][]zone.Record{}
	for index, want := range desired {
		ttl := want.TTL
		if ttl == 0 {
			ttl = defaultTTL(want.Type)
		}
		record, err := zone.NormalizeRecord(zoneName, want.Name, want.Type, ttl, want.Value)
		if err != nil {
			return nil, fmt.Errorf("desired[%d]: %w", index, err)
		}
		record.Source = source
		key := recordKey{name: record.Name, kind: record.Type}
		if slices.ContainsFunc(wanted[key], func(existing zone.Record) bool {
			return valueEqual(record.Type, existing.Value, record.Value)
		}) {
			continue
		}
		wanted[key] = append(wanted[key], record)
	}
	return wanted, nil
}

func planCnameConflicts(wanted map[recordKey][]zone.Record, existingByName map[string][]zone.Record, addConflict func(zone.Record, string)) {
	cnameNames := map[string]struct{}{}
	otherNames := map[string]struct{}{}
	for key := range wanted {
		if key.kind == zone.TypeCNAME {
			cnameNames[key.name] = struct{}{}
			continue
		}
		otherNames[key.name] = struct{}{}
	}
	for name := range cnameNames {
		for _, record := range existingByName[name] {
			if record.Type == zone.TypeCNAME {
				continue
			}
			addConflict(record, "prevents the alias at the same name")
		}
	}
	for name := range otherNames {
		for _, record := range existingByName[name] {
			if record.Type != zone.TypeCNAME {
				continue
			}
			addConflict(record, "is an alias, so no other record can live at this name")
		}
	}
}

// effectiveTTL is the TTL every member of the RRset will carry. An RRset must
// use one TTL, so an existing non-zero TTL always wins — including the TTL of a
// record that is about to be replaced, so the plan stays applicable whether or
// not the caller replaces conflicts.
func effectiveTTL(key recordKey, members []zone.Record, desired []zone.Record) uint32 {
	for _, member := range members {
		if member.TTL != 0 {
			return member.TTL
		}
	}
	for _, want := range desired {
		if want.TTL != 0 {
			return want.TTL
		}
	}
	return defaultTTL(key.kind)
}

func findValue(members []zone.Record, want zone.Record, source string) (zone.Record, bool) {
	for _, member := range members {
		if member.Source != source {
			continue
		}
		if valueEqual(want.Type, member.Value, want.Value) {
			return member, true
		}
	}
	return zone.Record{}, false
}

// conflictingUserRecord decides whether a user record that shares an RRset with
// the desired set has to go. A/AAAA/CNAME/MX/SRV are exclusive: the engine owns
// the whole answer. TXT is not — a domain-verification TXT at the apex is none
// of our business — so only a TXT of the same kind (SPF, DMARC, DKIM)
// conflicts.
//
// SRV is exclusive for the same reason MX is: an RFC 2782 client picks
// uniformly at random between equal-priority, equal-weight targets, so leaving
// the old provider's `_imaps._tcp` beside ours sends half of every client
// autoconfiguration to a server that has no such mailbox. Only the owners the
// engine itself publishes are ever considered — this function is reached once
// per desired (name, type) — so unrelated SRV records elsewhere in the zone are
// left alone.
func conflictingUserRecord(key recordKey, member zone.Record, desired []zone.Record) bool {
	switch key.kind {
	case zone.TypeA, zone.TypeAAAA, zone.TypeCNAME, zone.TypeMX:
		return true
	case zone.TypeSRV:
		return len(desired) > 0
	case zone.TypeTXT:
		kind := txtKind(member.Value)
		if kind == "" {
			return false
		}
		return slices.ContainsFunc(desired, func(want zone.Record) bool { return txtKind(want.Value) == kind })
	default:
		return false
	}
}

func conflictReason(kind zone.RecordType) string {
	switch kind {
	case zone.TypeMX:
		return "sends mail somewhere else"
	case zone.TypeTXT:
		return "is a different policy record of the same kind"
	case zone.TypeSRV:
		return "points mail clients somewhere else"
	default:
		return "points somewhere else"
	}
}

func txtKind(value string) string {
	text := strings.ToLower(strings.TrimSpace(TxtText(value)))
	switch {
	case strings.HasPrefix(text, "v=spf1"):
		return "spf"
	case strings.HasPrefix(text, "v=dmarc1"):
		return "dmarc"
	case strings.HasPrefix(text, "v=dkim1"):
		return "dkim"
	default:
		return ""
	}
}

// defaultPolicyTTL is the TTL used for mail policy records (MX, SPF, DKIM,
// DMARC) when the RRset does not already prescribe one.
const defaultPolicyTTL = 3600

func defaultTTL(kind zone.RecordType) uint32 {
	switch kind {
	case zone.TypeMX, zone.TypeTXT:
		return defaultPolicyTTL
	default:
		return zone.DefaultRecordTTL
	}
}

func valueEqual(kind zone.RecordType, left, right string) bool {
	switch kind {
	case zone.TypeA, zone.TypeAAAA:
		leftAddr, leftErr := netip.ParseAddr(left)
		rightAddr, rightErr := netip.ParseAddr(right)
		if leftErr != nil || rightErr != nil {
			return left == right
		}
		return leftAddr.Unmap() == rightAddr.Unmap()
	case zone.TypeCNAME, zone.TypeNS:
		return equalTarget(left, right)
	case zone.TypeMX:
		leftFields := strings.Fields(left)
		rightFields := strings.Fields(right)
		if len(leftFields) != 2 || len(rightFields) != 2 {
			return left == right
		}
		return leftFields[0] == rightFields[0] && equalTarget(leftFields[1], rightFields[1])
	case zone.TypeTXT:
		return TxtEqual(left, right)
	default:
		return left == right
	}
}

func equalTarget(left, right string) bool {
	return strings.EqualFold(strings.TrimSuffix(left, "."), strings.TrimSuffix(right, "."))
}

func ttlWhy(from, to uint32) string {
	return fmt.Sprintf("TTL %d → %d so the RRset accepts new members", from, to)
}

func change(op string, record zone.Record, recordID, previous, why string) RecordChange {
	return RecordChange{
		Op:            op,
		Name:          record.Name,
		Type:          string(record.Type),
		Value:         record.Value,
		TTL:           record.TTL,
		RecordID:      recordID,
		PreviousValue: previous,
		Why:           why,
	}
}

func desiredKeys(wanted map[recordKey][]zone.Record) []recordKey {
	keys := make([]recordKey, 0, len(wanted))
	for key := range wanted {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b recordKey) int {
		if a.name != b.name {
			if a.name == "@" {
				return -1
			}
			if b.name == "@" {
				return 1
			}
			return strings.Compare(a.name, b.name)
		}
		return strings.Compare(string(a.kind), string(b.kind))
	})
	return keys
}

func sortedConflicts(conflicts map[string]Conflict) []Conflict {
	result := make([]Conflict, 0, len(conflicts))
	for _, conflict := range conflicts {
		result = append(result, conflict)
	}
	slices.SortFunc(result, func(a, b Conflict) int {
		if a.Name != b.Name {
			return strings.Compare(a.Name, b.Name)
		}
		if a.Type != b.Type {
			return strings.Compare(string(a.Type), string(b.Type))
		}
		return strings.Compare(a.Value, b.Value)
	})
	return result
}
