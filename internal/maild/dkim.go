package maild

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// DKIM: key generation, publication and signing.
//
// Three things live here, and they have to agree with each other or mail is
// worse off than if it were unsigned:
//
//  1. generation — one keypair per algorithm per domain, private half stored
//     through the Store, public half kept in the form the record needs;
//  2. publication — the TXT record text the facade parses back out of
//     dnsZoneFile. internal/mail/zonefile.go refuses a record whose v= is not
//     first, whose k= is missing, or whose p= is not base64, and a refused
//     record is reported as mail.records.invalid and its algorithm dropped from
//     the readiness set, so the customer's binding never completes. The value
//     rendered here is the one validDkimValue accepts;
//  3. signing — relaxed/relaxed canonicalisation over the standard header set.
//
// A receiver that finds a published key and a signature it does not verify
// treats the message as *failing* authentication, which is strictly worse than
// no DKIM at all. That is why the record and the signer are in one file: the
// selector, the algorithm tag and the public key are written once and used by
// both.
// -----------------------------------------------------------------------------

// DkimRSABits is the modulus size for a new RSA signing key. 2048 is the
// practical maximum: the public key has to fit in a TXT record, and 4096-bit
// keys are known to be dropped by resolvers and by some receivers' record
// parsers. 1024 is still accepted everywhere but is below current guidance.
const DkimRSABits = 2048

// DKIM signature algorithm tags — the a= tag of the signature header, which is
// a different spelling from the JMAP "@type" (DkimAlgorithmRSA) and from the
// record's k= tag (DkimKeyType).
const (
	DkimSignatureAlgorithmRSA     = "rsa-sha256"
	DkimSignatureAlgorithmEd25519 = "ed25519-sha256"
)

// dkimSignatureAlgorithm maps a stored algorithm onto its a= tag.
var dkimSignatureAlgorithm = map[string]string{
	DkimAlgorithmRSA:     DkimSignatureAlgorithmRSA,
	DkimAlgorithmEd25519: DkimSignatureAlgorithmEd25519,
}

// maxCharacterString is the largest a single TXT character-string may be. A
// longer value has to be split, and the split form is equal to the joined form
// on the wire, which is how the facade compares records.
const maxCharacterString = 255

// maxSelectorLabel is a DNS label's limit. The selector is the first label of
// the published owner name, so a longer one could never be published.
const maxSelectorLabel = 63

// -----------------------------------------------------------------------------
// Generation
// -----------------------------------------------------------------------------

// DkimOptions describes the keys a domain is created with. It is the JMAP
// dkimManagement object in Go form: the facade sends a selector template and
// three durations on every x:Domain/set create, and this is where they land.
type DkimOptions struct {
	// SelectorTemplate is the JMAP selectorTemplate. Empty means
	// DefaultSelectorTemplate, which is what the facade always sends.
	SelectorTemplate string
	// Algorithms is the set to generate. Empty means RequestedDkimAlgorithms —
	// both of them, because the facade's bind step waits for one active
	// signature per algorithm before a binding leaves DEGRADED.
	Algorithms []string
	// Version is the {version} substitution, from 1. Zero means 1.
	Version int
	// Now is the creation time and the {date-…} substitution. Zero means
	// time.Now. It is UTC-normalised, because the selector must not change
	// depending on where the process runs.
	Now time.Time
	// RotateAfter, RetireAfter and DeleteAfter are the lifetimes the facade
	// sends in milliseconds (90 d / 7 d / 30 d). They are recorded on the key
	// for a future rotation pass; nothing here acts on them yet.
	RotateAfter time.Duration
	RetireAfter time.Duration
	DeleteAfter time.Duration
}

// normalize fills the defaults, so every entry point applies the same ones.
func (o DkimOptions) normalize() DkimOptions {
	if strings.TrimSpace(o.SelectorTemplate) == "" {
		o.SelectorTemplate = DefaultSelectorTemplate
	}
	if len(o.Algorithms) == 0 {
		o.Algorithms = RequestedDkimAlgorithms
	}
	if o.Version <= 0 {
		o.Version = 1
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	o.Now = o.Now.UTC()
	return o
}

// GenerateDkimKey generates one signing keypair for a domain.
//
// The key is returned active, not pending: the facade polls ListDkim for at
// most 20 s after creating a domain and wants one active signature per
// algorithm, so a key that has to be promoted by a later pass shows the
// operator a degraded binding for no reason.
func GenerateDkimKey(domainID, algorithm string, options DkimOptions) (DkimKey, error) {
	options = options.normalize()
	if strings.TrimSpace(domainID) == "" {
		return DkimKey{}, fmt.Errorf("%w: a DKIM key needs a domain", ErrInvalid)
	}
	selector, err := ExpandDkimSelector(options.SelectorTemplate, options.Version, algorithm, options.Now)
	if err != nil {
		return DkimKey{}, err
	}
	public, private, err := generateDkimKeypair(algorithm)
	if err != nil {
		return DkimKey{}, err
	}

	key := NewDkimKey(domainID, selector, algorithm, options.Now)
	key.Version = options.Version
	key.Stage = DkimStageActive
	key.PublicKey = public
	key.PrivateKey = private
	if options.RotateAfter > 0 {
		key.RotateAt = options.Now.Add(options.RotateAfter)
	}
	if options.RetireAfter > 0 {
		key.RetireAt = options.Now.Add(options.RetireAfter)
	}
	if options.DeleteAfter > 0 {
		key.DeleteAt = options.Now.Add(options.DeleteAfter)
	}
	return key, nil
}

// GenerateDkimKeys generates one key per requested algorithm, ordered by
// selector to match the order the facade's client sorts into.
func GenerateDkimKeys(domainID string, options DkimOptions) ([]DkimKey, error) {
	options = options.normalize()
	keys := make([]DkimKey, 0, len(options.Algorithms))
	seen := make(map[string]string, len(options.Algorithms))
	for _, algorithm := range options.Algorithms {
		key, err := GenerateDkimKey(domainID, algorithm, options)
		if err != nil {
			return nil, err
		}
		// Two algorithms sharing a selector would share a DkimID and the
		// second would silently overwrite the first, leaving the domain with
		// one algorithm and a binding that never completes. A template with no
		// {algorithm} placeholder does exactly that.
		if other, clash := seen[key.Selector]; clash {
			return nil, fmt.Errorf(
				"%w: DKIM selector template %q gives %s and %s the same selector %q",
				ErrInvalid, options.SelectorTemplate, other, algorithm, key.Selector)
		}
		seen[key.Selector] = algorithm
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Selector < keys[j].Selector })
	return keys, nil
}

// ProvisionDkimKeys generates a domain's keys and stores them, returning what it
// wrote. This is what x:Domain/set create calls, synchronously, before it
// answers: the client reads the domain back immediately and expects
// dnsZoneFile to already carry these records.
//
// The version is one past the highest already stored for the domain, so calling
// it again rotates rather than colliding.
func ProvisionDkimKeys(ctx context.Context, store *Store, domainID string, options DkimOptions) ([]DkimKey, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: a DKIM provision needs a store", ErrInvalid)
	}
	options = options.normalize()
	if options.Version <= 1 {
		existing, err := store.ListDkimKeys(ctx, domainID)
		if err != nil {
			return nil, err
		}
		options.Version = nextDkimVersion(existing)
	}

	keys, err := GenerateDkimKeys(domainID, options)
	if err != nil {
		return nil, err
	}
	stored := make([]DkimKey, 0, len(keys))
	for _, key := range keys {
		written, err := store.PutDkimKey(ctx, key)
		if err != nil {
			return nil, err
		}
		stored = append(stored, written)
	}
	return stored, nil
}

// nextDkimVersion is one past the highest version already stored, so a rotation
// gets a fresh selector instead of overwriting the key currently signing mail.
func nextDkimVersion(keys []DkimKey) int {
	highest := 0
	for _, key := range keys {
		if key.Version > highest {
			highest = key.Version
		}
	}
	return highest + 1
}

// generateDkimKeypair returns the base64 public key in the form the p= tag and
// the JMAP publicKey property both carry, and the PKCS#8 private half.
func generateDkimKeypair(algorithm string) (public string, private Secret, err error) {
	switch algorithm {
	case DkimAlgorithmRSA:
		key, keyErr := rsa.GenerateKey(rand.Reader, DkimRSABits)
		if keyErr != nil {
			return "", nil, fmt.Errorf("maild: generate an RSA DKIM key: %w", keyErr)
		}
		// SubjectPublicKeyInfo, standard base64, unwrapped — the
		// "MIIBIjANBgkqhkiG9w0BAQEFAAOC…" form every receiver expects and the
		// form the captured signature fixture carries.
		spki, marshalErr := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if marshalErr != nil {
			return "", nil, fmt.Errorf("maild: encode an RSA DKIM public key: %w", marshalErr)
		}
		pkcs8, marshalErr := x509.MarshalPKCS8PrivateKey(key)
		if marshalErr != nil {
			return "", nil, fmt.Errorf("maild: encode an RSA DKIM private key: %w", marshalErr)
		}
		return base64.StdEncoding.EncodeToString(spki), Secret(pkcs8), nil

	case DkimAlgorithmEd25519:
		publicKey, privateKey, keyErr := ed25519.GenerateKey(rand.Reader)
		if keyErr != nil {
			return "", nil, fmt.Errorf("maild: generate an Ed25519 DKIM key: %w", keyErr)
		}
		pkcs8, marshalErr := x509.MarshalPKCS8PrivateKey(privateKey)
		if marshalErr != nil {
			return "", nil, fmt.Errorf("maild: encode an Ed25519 DKIM private key: %w", marshalErr)
		}
		// RFC 8463: the record carries the raw 32-byte key, not an SPKI.
		return base64.StdEncoding.EncodeToString(publicKey), Secret(pkcs8), nil

	default:
		return "", nil, fmt.Errorf("%w: unknown DKIM algorithm %q", ErrInvalid, algorithm)
	}
}

// -----------------------------------------------------------------------------
// Selector templates
// -----------------------------------------------------------------------------

// ExpandDkimSelector expands a JMAP selectorTemplate. The facade always sends
// "v{version}-{algorithm}-{date-%Y%m%d}", which the captured probe shows
// expanding to "v1-rsa-20260903".
//
// {algorithm} is the short tag ("rsa", "ed25519"), never the "@type" name. An
// unknown placeholder is an error rather than a literal, because a literal
// brace is not a legal DNS label and the failure would only surface as an
// unpublishable record much later.
func ExpandDkimSelector(template string, version int, algorithm string, at time.Time) (string, error) {
	tag, known := DkimSelectorTag[algorithm]
	if !known {
		return "", fmt.Errorf("%w: unknown DKIM algorithm %q", ErrInvalid, algorithm)
	}
	if strings.TrimSpace(template) == "" {
		template = DefaultSelectorTemplate
	}
	if version <= 0 {
		version = 1
	}
	at = at.UTC()

	var out strings.Builder
	rest := template
	for {
		open := strings.Index(rest, "{")
		if open < 0 {
			out.WriteString(rest)
			break
		}
		out.WriteString(rest[:open])
		rest = rest[open+1:]
		end := strings.Index(rest, "}")
		if end < 0 {
			return "", fmt.Errorf("%w: unterminated placeholder in DKIM selector template %q", ErrInvalid, template)
		}
		token := rest[:end]
		rest = rest[end+1:]

		switch {
		case token == "version":
			out.WriteString(strconv.Itoa(version))
		case token == "algorithm":
			out.WriteString(tag)
		case strings.HasPrefix(token, "date-"):
			formatted, err := strftime(token[len("date-"):], at)
			if err != nil {
				return "", fmt.Errorf("%w: DKIM selector template %q: %s", ErrInvalid, template, err)
			}
			out.WriteString(formatted)
		default:
			return "", fmt.Errorf("%w: unknown DKIM selector placeholder {%s}", ErrInvalid, token)
		}
	}

	selector := strings.ToLower(out.String())
	if err := validateDkimSelector(selector); err != nil {
		return "", fmt.Errorf("%w: DKIM selector template %q produced %q: %s", ErrInvalid, template, selector, err)
	}
	return selector, nil
}

// validateDkimSelector refuses a selector that could not be the first label of
// the published owner name.
func validateDkimSelector(selector string) error {
	if selector == "" {
		return errEmptySelector
	}
	if len(selector) > maxSelectorLabel {
		return fmt.Errorf("a DNS label is at most %d bytes", maxSelectorLabel)
	}
	if strings.HasPrefix(selector, "-") || strings.HasSuffix(selector, "-") {
		return errSelectorHyphen
	}
	for index := 0; index < len(selector); index++ {
		character := selector[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '-':
		default:
			return fmt.Errorf("%q is not a letter, digit or hyphen", string(character))
		}
	}
	return nil
}

var (
	errEmptySelector  = fmt.Errorf("the selector is empty")
	errSelectorHyphen = fmt.Errorf("a DNS label cannot start or end with a hyphen")
)

// strftime expands the subset of strftime the selector template uses. The set
// is deliberately closed: an unrecognised verb is an error, not a passthrough,
// so a typo cannot silently become part of a selector that then has to live for
// the key's whole lifetime.
func strftime(format string, at time.Time) (string, error) {
	var out strings.Builder
	for index := 0; index < len(format); index++ {
		if format[index] != '%' {
			out.WriteByte(format[index])
			continue
		}
		index++
		if index >= len(format) {
			return "", fmt.Errorf("a trailing %% has no verb")
		}
		switch format[index] {
		case 'Y':
			out.WriteString(at.Format("2006"))
		case 'y':
			out.WriteString(at.Format("06"))
		case 'm':
			out.WriteString(at.Format("01"))
		case 'd':
			out.WriteString(at.Format("02"))
		case 'H':
			out.WriteString(at.Format("15"))
		case 'M':
			out.WriteString(at.Format("04"))
		case 'S':
			out.WriteString(at.Format("05"))
		case '%':
			out.WriteByte('%')
		default:
			return "", fmt.Errorf("unsupported strftime verb %%%s", string(format[index]))
		}
	}
	return out.String(), nil
}

// -----------------------------------------------------------------------------
// Publication
// -----------------------------------------------------------------------------

// DkimRecordName is the record owner relative to the domain,
// "<selector>._domainkey". parseDkimRecords cuts on exactly this, so the
// selector must be the whole first label.
func DkimRecordName(selector string) string {
	return selector + "._domainkey"
}

// DkimRecordValue renders the TXT value the facade publishes.
//
// The tag order is not cosmetic: validDkimValue requires v to be the *first*
// tag and to be DKIM1, requires k to be present (a missing k is refused even
// though RFC 6376 defaults it to rsa), and requires p to decode as base64.
// A value that fails any of those is not published, is reported as
// mail.records.invalid, and its algorithm is dropped from the readiness set —
// so the binding never completes and the operator sees no key at all.
func DkimRecordValue(key DkimKey) string {
	keyType, known := DkimKeyType[key.Algorithm]
	if !known {
		// Not renderable. An empty value is dropped by the parser, which is the
		// honest outcome: better no record than a record that fails to verify.
		return ""
	}
	return "v=DKIM1; k=" + keyType + "; h=sha256; p=" + key.PublicKey
}

// DkimZoneRecord renders one signing key as a BIND record line, in the layout
// the captured probe zone file uses: a single quoted string when it fits, and a
// parenthesised sequence of character-strings when it does not. The facade
// joins the strings back together before comparing, so the two forms are equal
// on the wire; the split exists because a TXT character-string is capped at 255
// bytes and an RSA-2048 key does not fit in one.
//
// The returned text has no trailing newline.
func DkimZoneRecord(key DkimKey, domainName string) string {
	value := DkimRecordValue(key)
	if value == "" {
		return ""
	}
	owner := DkimRecordName(key.Selector) + "." + NormalizeDomainName(domainName) + "."
	parts := splitCharacterStrings(value, maxCharacterString)
	if len(parts) == 1 {
		return owner + ` IN TXT "` + parts[0] + `"`
	}
	var out strings.Builder
	out.WriteString(owner)
	out.WriteString(" IN TXT (")
	for _, part := range parts {
		out.WriteString("\n    \"")
		out.WriteString(part)
		out.WriteString("\"")
	}
	out.WriteString("\n)")
	return out.String()
}

// DkimZoneRecords renders every active key, newline-terminated, ready to splice
// into dnsZoneFile. Keys that are not active are skipped: the facade publishes
// only active signatures, and publishing a retired key's record would invite
// receivers to verify signatures this server no longer makes.
func DkimZoneRecords(keys []DkimKey, domainName string) string {
	var out strings.Builder
	for _, key := range keys {
		if !key.Active() {
			continue
		}
		record := DkimZoneRecord(key, domainName)
		if record == "" {
			continue
		}
		out.WriteString(record)
		out.WriteString("\n")
	}
	return out.String()
}

// splitCharacterStrings cuts a value into TXT character-strings of at most
// limit bytes. It splits on bytes, not runes, because a character-string is
// length-prefixed in octets; every value here is base64 and ASCII tags anyway.
func splitCharacterStrings(value string, limit int) []string {
	if limit <= 0 || len(value) <= limit {
		return []string{value}
	}
	parts := make([]string, 0, (len(value)+limit-1)/limit)
	for len(value) > limit {
		parts = append(parts, value[:limit])
		value = value[limit:]
	}
	return append(parts, value)
}

// -----------------------------------------------------------------------------
// Signing
// -----------------------------------------------------------------------------

// DefaultSignedHeaders is the header set a signature covers. Only the headers
// actually present in the message are listed in h= and hashed.
//
// From is mandatory (RFC 6376 §5.4) and is what a receiver's alignment check
// compares against d=. The rest are the fields whose alteration would change
// what the message means to a reader — a signature that covered only From would
// verify happily while somebody rewrote the Subject.
//
// Bcc, Return-Path and Received are deliberately absent: they are added,
// removed or rewritten in transit by design, so signing them would guarantee
// failures.
var DefaultSignedHeaders = []string{
	"from",
	"sender",
	"reply-to",
	"subject",
	"date",
	"message-id",
	"to",
	"cc",
	"mime-version",
	"content-type",
	"content-transfer-encoding",
	"in-reply-to",
	"references",
	"list-unsubscribe",
}

// SignOptions tunes a signature. The zero value signs DefaultSignedHeaders at
// time.Now with no expiry, which is what outbound mail wants.
type SignOptions struct {
	// Headers replaces DefaultSignedHeaders. Names are matched
	// case-insensitively and only signed when present.
	Headers []string
	// Now is the t= tag. Zero means time.Now.
	Now time.Time
	// Expires sets the x= tag, t + Expires. Zero omits x=; that is the default
	// because an expired signature is indistinguishable from a forged one to a
	// receiver, and mail sits in queues for days.
	Expires time.Duration
}

// DkimSigner signs messages for one domain with one key. It parses the private
// key once, so the per-message path is a hash and a signature.
//
// The private key never leaves this struct: it is held as a parsed crypto key,
// no accessor returns it, and DkimKey.PrivateKey is a Secret that redacts under
// every fmt verb, so a %v of the key that built this signer prints nothing.
type DkimSigner struct {
	domain    string
	selector  string
	algorithm string
	rsaKey    *rsa.PrivateKey
	ed25519   ed25519.PrivateKey
	headers   []string
	now       func() time.Time
	expires   time.Duration
}

// NewDkimSigner parses a stored key into a signer for domainName.
func NewDkimSigner(domainName string, key DkimKey, options SignOptions) (*DkimSigner, error) {
	domain := NormalizeDomainName(domainName)
	if domain == "" {
		return nil, fmt.Errorf("%w: a DKIM signer needs a domain", ErrInvalid)
	}
	if key.Selector == "" {
		return nil, fmt.Errorf("%w: a DKIM signer needs a selector", ErrInvalid)
	}
	algorithm, known := dkimSignatureAlgorithm[key.Algorithm]
	if !known {
		return nil, fmt.Errorf("%w: unknown DKIM algorithm %q", ErrInvalid, key.Algorithm)
	}
	if len(key.PrivateKey) == 0 {
		// Deliberately does not name the key material, only the selector.
		return nil, fmt.Errorf("%w: DKIM selector %q has no private key", ErrInvalid, key.Selector)
	}

	parsed, err := x509.ParsePKCS8PrivateKey(key.PrivateKey.Bytes())
	if err != nil {
		// The error from x509 describes structure, not content, but wrapping it
		// with the selector rather than the key keeps that guarantee local.
		return nil, fmt.Errorf("maild: parse the DKIM private key for selector %q: %w", key.Selector, err)
	}

	signer := &DkimSigner{
		domain:    domain,
		selector:  key.Selector,
		algorithm: algorithm,
		headers:   normalizeSignedHeaders(options.Headers),
		now:       time.Now,
		expires:   options.Expires,
	}
	if !options.Now.IsZero() {
		at := options.Now
		signer.now = func() time.Time { return at }
	}

	switch typed := parsed.(type) {
	case *rsa.PrivateKey:
		if key.Algorithm != DkimAlgorithmRSA {
			return nil, fmt.Errorf("%w: selector %q is %s but holds an RSA key", ErrInvalid, key.Selector, key.Algorithm)
		}
		signer.rsaKey = typed
	case ed25519.PrivateKey:
		if key.Algorithm != DkimAlgorithmEd25519 {
			return nil, fmt.Errorf("%w: selector %q is %s but holds an Ed25519 key", ErrInvalid, key.Selector, key.Algorithm)
		}
		signer.ed25519 = typed
	default:
		return nil, fmt.Errorf("%w: selector %q holds a key type DKIM cannot sign with", ErrInvalid, key.Selector)
	}
	return signer, nil
}

// Domain is the d= tag this signer claims.
func (s *DkimSigner) Domain() string { return s.domain }

// Selector is the s= tag this signer claims.
func (s *DkimSigner) Selector() string { return s.selector }

// normalizeSignedHeaders lowercases and de-duplicates the signed header set,
// preserving order. Duplicates in h= would ask a verifier for a second
// occurrence of a header that has only one, and the whole signature fails.
func normalizeSignedHeaders(headers []string) []string {
	if len(headers) == 0 {
		headers = DefaultSignedHeaders
	}
	out := make([]string, 0, len(headers))
	seen := make(map[string]struct{}, len(headers))
	for _, header := range headers {
		name := strings.ToLower(strings.TrimSpace(header))
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// Sign returns message with a DKIM-Signature header prepended. The message is
// normalised to CRLF line endings first, because that is what the signature
// covers and what an SMTP client must transmit.
func (s *DkimSigner) Sign(message []byte) ([]byte, error) {
	normalized := normalizeCRLF(message)
	header, err := s.signatureHeader(normalized)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(header)+2+len(normalized))
	out = append(out, header...)
	out = append(out, '\r', '\n')
	out = append(out, normalized...)
	return out, nil
}

// SignatureHeader returns the folded DKIM-Signature header, without its
// trailing CRLF, for a caller that writes headers itself.
func (s *DkimSigner) SignatureHeader(message []byte) (string, error) {
	return s.signatureHeader(normalizeCRLF(message))
}

// signatureHeader does the work, on a message already normalised to CRLF.
func (s *DkimSigner) signatureHeader(message []byte) (string, error) {
	fields, body := splitMessage(message)

	selected, names := selectSignedHeaders(fields, s.headers)
	if !containsFold(names, "from") {
		// RFC 6376 §5.4: From is the one header a signature must cover. A
		// signature without it is invalid and every receiver rejects it, so
		// refusing here turns a silent authentication failure into an error at
		// the point the message was built.
		return "", fmt.Errorf("%w: a message cannot be DKIM-signed without a From header", ErrInvalid)
	}

	bodyHash := sha256.Sum256(relaxedBody(body))

	at := s.now().UTC()
	tags := "v=1" +
		"; a=" + s.algorithm +
		"; c=relaxed/relaxed" +
		"; d=" + s.domain +
		"; s=" + s.selector +
		"; t=" + strconv.FormatInt(at.Unix(), 10)
	if s.expires > 0 {
		tags += "; x=" + strconv.FormatInt(at.Add(s.expires).Unix(), 10)
	}
	tags += "; h=" + strings.Join(names, ":") +
		"; bh=" + base64.StdEncoding.EncodeToString(bodyHash[:]) +
		"; b="

	// The data signed is every selected header, relaxed-canonicalised and CRLF
	// terminated, then this signature header itself relaxed-canonicalised with
	// an empty b= and *no* trailing CRLF (RFC 6376 §3.7). A verifier rebuilds
	// exactly this by stripping the b= value back out.
	var signed strings.Builder
	for _, field := range selected {
		signed.WriteString(relaxedHeader(field))
		signed.WriteString("\r\n")
	}
	signed.WriteString("dkim-signature:")
	signed.WriteString(collapseWSP(tags))

	digest := sha256.Sum256([]byte(signed.String()))
	signature, err := s.sign(digest)
	if err != nil {
		return "", err
	}

	// The b= value is emitted in space-separated chunks so the folder has
	// somewhere to break. Whitespace inside a base64 tag value is ignored by
	// every verifier, and the value is removed wholesale before the header is
	// re-canonicalised, so the folding cannot change what was signed.
	chunks := splitCharacterStrings(base64.StdEncoding.EncodeToString(signature), 64)
	return foldSignatureHeader(tags + strings.Join(chunks, " ")), nil
}

// sign produces the raw signature over the header digest.
func (s *DkimSigner) sign(digest [sha256.Size]byte) ([]byte, error) {
	switch {
	case s.rsaKey != nil:
		signature, err := rsa.SignPKCS1v15(rand.Reader, s.rsaKey, crypto.SHA256, digest[:])
		if err != nil {
			return nil, fmt.Errorf("maild: sign with DKIM selector %q: %w", s.selector, err)
		}
		return signature, nil
	case s.ed25519 != nil:
		// RFC 8463: ed25519-sha256 signs the SHA-256 hash of the header data
		// with PureEdDSA, not the header data itself.
		return ed25519.Sign(s.ed25519, digest[:]), nil
	default:
		return nil, fmt.Errorf("%w: DKIM selector %q has no usable key", ErrInvalid, s.selector)
	}
}

// SignMessage signs a message once per active key, which is how a domain offers
// both an RSA signature (the only one Gmail and Microsoft verify) and an
// Ed25519 one in the same message. Inactive keys are skipped; if no key is
// active the message is returned unchanged, because unsigned mail is delivered
// and mail that could not be sent is not.
func SignMessage(message []byte, domainName string, keys []DkimKey, options SignOptions) ([]byte, error) {
	signed := normalizeCRLF(message)
	for _, key := range keys {
		if !key.Active() {
			continue
		}
		signer, err := NewDkimSigner(domainName, key, options)
		if err != nil {
			return nil, err
		}
		signed, err = signer.Sign(signed)
		if err != nil {
			return nil, err
		}
	}
	return signed, nil
}

// -----------------------------------------------------------------------------
// Message parsing and canonicalisation
// -----------------------------------------------------------------------------

// headerField is one header as it appears in the message, continuation lines
// included and the trailing CRLF removed.
type headerField struct {
	name string
	raw  string
}

// splitMessage cuts a CRLF-normalised message into its header fields and body.
// A message with no blank line is all headers and an empty body.
func splitMessage(message []byte) ([]headerField, []byte) {
	text := string(message)
	var headerText, body string
	if index := strings.Index(text, "\r\n\r\n"); index >= 0 {
		headerText = text[:index]
		body = text[index+4:]
	} else {
		headerText = strings.TrimSuffix(text, "\r\n")
	}
	if headerText == "" {
		return nil, []byte(body)
	}

	var fields []headerField
	for _, line := range strings.Split(headerText, "\r\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(fields) == 0 {
				// A continuation with nothing to continue. Dropping it is what
				// the RFC 5322 "obs-" parsers do and keeps the field list sane.
				continue
			}
			fields[len(fields)-1].raw += "\r\n" + line
			continue
		}
		name, _, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields = append(fields, headerField{
			name: strings.ToLower(strings.TrimSpace(name)),
			raw:  line,
		})
	}
	return fields, []byte(body)
}

// selectSignedHeaders picks the fields to sign, in h= order.
//
// When a name appears more than once, the *bottom-most* occurrence is the one
// hashed: RFC 6376 §5.4.2 has verifiers walk the header block from the bottom,
// so signing the top one would produce a signature that never verifies.
func selectSignedHeaders(fields []headerField, wanted []string) ([]headerField, []string) {
	selected := make([]headerField, 0, len(wanted))
	names := make([]string, 0, len(wanted))
	for _, name := range wanted {
		for index := len(fields) - 1; index >= 0; index-- {
			if fields[index].name != name {
				continue
			}
			selected = append(selected, fields[index])
			names = append(names, name)
			break
		}
	}
	return selected, names
}

// containsFold reports whether values holds name, compared case-insensitively.
func containsFold(values []string, name string) bool {
	for _, value := range values {
		if strings.EqualFold(value, name) {
			return true
		}
	}
	return false
}

// relaxedHeader is RFC 6376 §3.4.2: lowercase the name, unfold the value,
// collapse WSP runs to one SP, drop WSP around the colon and at the end. The
// result carries no trailing CRLF; the caller adds one.
func relaxedHeader(field headerField) string {
	_, value, found := strings.Cut(field.raw, ":")
	if !found {
		return field.name + ":"
	}
	unfolded := strings.ReplaceAll(value, "\r\n", "")
	return field.name + ":" + strings.TrimSpace(collapseWSP(unfolded))
}

// relaxedBody is RFC 6376 §3.4.4: collapse WSP runs within a line, drop
// trailing WSP on every line, drop trailing empty lines entirely, and terminate
// what is left with exactly one CRLF.
//
// A body that is empty after that is canonicalised as a null input, not as a
// bare CRLF — the difference is a different bh= and therefore every signature
// on an empty-bodied message failing.
func relaxedBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	lines := strings.Split(string(body), "\r\n")
	for index := range lines {
		lines[index] = strings.TrimRight(collapseWSP(lines[index]), " ")
	}
	end := len(lines)
	for end > 0 && lines[end-1] == "" {
		end--
	}
	if end == 0 {
		return nil
	}
	return []byte(strings.Join(lines[:end], "\r\n") + "\r\n")
}

// collapseWSP reduces every run of spaces and tabs to a single space. It leaves
// CRLF alone, so callers unfold first when they mean to.
func collapseWSP(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	space := false
	for index := 0; index < len(text); index++ {
		character := text[index]
		if character == ' ' || character == '\t' {
			space = true
			continue
		}
		if space {
			out.WriteByte(' ')
			space = false
		}
		out.WriteByte(character)
	}
	if space {
		out.WriteByte(' ')
	}
	return out.String()
}

// normalizeCRLF rewrites bare LF as CRLF and leaves existing CRLF alone, so a
// message assembled with Go string literals signs and transmits identically to
// one read off the wire.
func normalizeCRLF(message []byte) []byte {
	if !hasBareLF(message) {
		return message
	}
	out := make([]byte, 0, len(message)+len(message)/16)
	for index := 0; index < len(message); index++ {
		character := message[index]
		if character == '\n' && (index == 0 || message[index-1] != '\r') {
			out = append(out, '\r')
		}
		out = append(out, character)
	}
	return out
}

// hasBareLF keeps normalizeCRLF allocation-free for the common case of a
// message that is already correct.
func hasBareLF(message []byte) bool {
	for index := 0; index < len(message); index++ {
		if message[index] == '\n' && (index == 0 || message[index-1] != '\r') {
			return true
		}
	}
	return false
}

// foldSignatureHeader wraps the tag list onto continuation lines. It only ever
// replaces an existing space with CRLF + TAB, so unfolding and collapsing the
// result reproduces the single-line form that was signed.
func foldSignatureHeader(tags string) string {
	const limit = 78
	var out strings.Builder
	line := "DKIM-Signature:"
	for _, token := range strings.Split(tags, " ") {
		if token == "" {
			continue
		}
		if len(line)+1+len(token) > limit && line != "DKIM-Signature:" {
			out.WriteString(line)
			out.WriteString("\r\n")
			line = "\t" + token
			continue
		}
		line += " " + token
	}
	out.WriteString(line)
	return out.String()
}
