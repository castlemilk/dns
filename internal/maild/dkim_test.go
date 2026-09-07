package maild

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// referenceTime is the probe capture's creation date, so the selectors these
// tests expect are the ones in internal/mail/stalwart/testdata.
var referenceTime = time.Date(2026, 9, 3, 8, 52, 28, 0, time.UTC)

// -----------------------------------------------------------------------------
// An independent DKIM verifier.
//
// Everything in this block is written from RFC 6376 §3.4, §3.5 and §3.7 and RFC
// 8463 §3 without calling anything in dkim.go: different structure, regexp
// rather than a byte loop, and it reads only the message text and the published
// record. That is the point — a verifier that shared the signer's
// canonicalisation would agree with a wrong signer, and the failure mode this
// guards against (a signature that a receiver rejects, which is worse for
// deliverability than no signature at all) is exactly the one a self-consistent
// test cannot see.
// -----------------------------------------------------------------------------

var (
	wspRun     = regexp.MustCompile(`[ \t]+`)
	bTagValue  = regexp.MustCompile(`(^|;)([ \t]*b=)[^;]*`)
	errNoField = errors.New("the message has no DKIM-Signature header")
)

type verifyField struct {
	name string
	raw  string
}

// splitForVerify cuts a message at its first blank line.
func splitForVerify(message string) (fields []verifyField, body string) {
	block := message
	if index := strings.Index(message, "\r\n\r\n"); index >= 0 {
		block = message[:index]
		body = message[index+4:]
	}
	for _, line := range strings.Split(block, "\r\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if len(fields) > 0 {
				fields[len(fields)-1].raw += "\r\n" + line
			}
			continue
		}
		name, _, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields = append(fields, verifyField{name: strings.ToLower(strings.Trim(name, " \t")), raw: line})
	}
	return fields, body
}

// canonHeaderRFC is the relaxed header canonicalisation of RFC 6376 §3.4.2.
func canonHeaderRFC(raw string) string {
	name, value, found := strings.Cut(raw, ":")
	if !found {
		return strings.ToLower(strings.Trim(raw, " \t")) + ":"
	}
	value = strings.ReplaceAll(value, "\r\n", "")
	value = wspRun.ReplaceAllString(value, " ")
	return strings.ToLower(strings.Trim(name, " \t")) + ":" + strings.Trim(value, " ")
}

// canonBodyRFC is the relaxed body canonicalisation of RFC 6376 §3.4.4.
func canonBodyRFC(body string) string {
	if body == "" {
		return ""
	}
	lines := strings.Split(body, "\r\n")
	for index, line := range lines {
		lines[index] = strings.TrimRight(wspRun.ReplaceAllString(line, " "), " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

// parseTagList reads a "k=v; k=v" tag list, unfolding first.
func parseTagList(value string) map[string]string {
	tags := map[string]string{}
	for _, part := range strings.Split(strings.ReplaceAll(value, "\r\n", ""), ";") {
		key, tagValue, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		tags[strings.Trim(key, " \t")] = strings.Trim(tagValue, " \t")
	}
	return tags
}

// verifyDkim checks the topmost DKIM-Signature of message against the public
// key carried by recordValue — the TXT record this server publishes — and
// reports the first thing that does not hold.
func verifyDkim(message, recordValue string) error {
	fields, body := splitForVerify(message)

	signatureRaw := ""
	for _, field := range fields {
		if field.name == "dkim-signature" {
			signatureRaw = field.raw
			break
		}
	}
	if signatureRaw == "" {
		return errNoField
	}
	_, signatureValue, _ := strings.Cut(signatureRaw, ":")
	tags := parseTagList(signatureValue)

	if tags["v"] != "1" {
		return fmt.Errorf("v= is %q, want 1", tags["v"])
	}
	if tags["c"] != "relaxed/relaxed" {
		return fmt.Errorf("c= is %q, want relaxed/relaxed", tags["c"])
	}

	bodyHash := sha256.Sum256([]byte(canonBodyRFC(body)))
	if got := base64.StdEncoding.EncodeToString(bodyHash[:]); got != tags["bh"] {
		return fmt.Errorf("bh= is %q, but the canonicalised body hashes to %q", tags["bh"], got)
	}

	// The signed data: every header named in h=, bottom-most occurrence first
	// for repeats, then this header with its b= value removed and no CRLF.
	var data strings.Builder
	consumed := map[string]int{}
	for _, name := range strings.Split(tags["h"], ":") {
		name = strings.ToLower(strings.Trim(name, " \t"))
		if name == "" {
			continue
		}
		skip := consumed[name]
		index := -1
		for candidate := len(fields) - 1; candidate >= 0; candidate-- {
			if fields[candidate].name != name {
				continue
			}
			if skip > 0 {
				skip--
				continue
			}
			index = candidate
			break
		}
		if index < 0 {
			continue
		}
		consumed[name]++
		data.WriteString(canonHeaderRFC(fields[index].raw))
		data.WriteString("\r\n")
	}
	data.WriteString(bTagValue.ReplaceAllString(canonHeaderRFC(signatureRaw), "${1}${2}"))
	digest := sha256.Sum256([]byte(data.String()))

	signature, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(tags["b"]), ""))
	if err != nil {
		return fmt.Errorf("b= is not base64: %w", err)
	}

	published, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(parseTagList(recordValue)["p"]), ""))
	if err != nil {
		return fmt.Errorf("the record's p= is not base64: %w", err)
	}

	switch tags["a"] {
	case DkimSignatureAlgorithmRSA:
		parsed, parseErr := x509.ParsePKIXPublicKey(published)
		if parseErr != nil {
			return fmt.Errorf("the record's p= is not a SubjectPublicKeyInfo: %w", parseErr)
		}
		public, ok := parsed.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("the record's p= is a %T, not an RSA key", parsed)
		}
		if verifyErr := rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature); verifyErr != nil {
			return fmt.Errorf("the RSA signature does not verify: %w", verifyErr)
		}
	case DkimSignatureAlgorithmEd25519:
		if len(published) != ed25519.PublicKeySize {
			return fmt.Errorf("the record's p= is %d bytes, want %d", len(published), ed25519.PublicKeySize)
		}
		if !ed25519.Verify(ed25519.PublicKey(published), digest[:], signature) {
			return errors.New("the Ed25519 signature does not verify")
		}
	default:
		return fmt.Errorf("unsupported a= %q", tags["a"])
	}
	return nil
}

// -----------------------------------------------------------------------------
// Canonicalisation
// -----------------------------------------------------------------------------

// TestRelaxedBodyCanonicalization pins the body hashes. The expected digests
// were produced by an independent Python implementation of RFC 6376 §3.4.4 and
// pasted here, so this test compares against something written outside Go
// rather than against itself.
func TestRelaxedBodyCanonicalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		canonical string
		bodyHash  string
	}{
		{
			name:      "collapses runs and drops trailing blank lines",
			body:      "Hello,  world.  \r\n\r\nThis is a test.\r\n\r\n\r\n",
			canonical: "Hello, world.\r\n\r\nThis is a test.\r\n",
			bodyHash:  "bV0iQZtr5NSPu2llDqxKe4cmOcPlKcwYV+xq548DT5Q=",
		},
		{
			// RFC 6376 §3.4.4: an empty body canonicalises to a null input, not
			// to a bare CRLF. Getting this wrong changes bh= for every
			// empty-bodied message, and read receipts have empty bodies.
			name:      "an empty body is a null input",
			body:      "",
			canonical: "",
			bodyHash:  "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=",
		},
		{
			name:      "a body of only newlines is also a null input",
			body:      "\r\n\r\n",
			canonical: "",
			bodyHash:  "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=",
		},
		{
			name:      "tabs collapse and a missing final CRLF is added",
			body:      "a\tb \t c\r\nno trailing newline",
			canonical: "a b c\r\nno trailing newline\r\n",
			bodyHash:  "Z8YFGFleMHxVLW9EsqdJnQKngxVBi0xsF3oFQDA8cO8=",
		},
		{
			name:      "an already canonical body is unchanged",
			body:      "body\r\n",
			canonical: "body\r\n",
			bodyHash:  "Ck5SoRNWUpSR4X0COv7R5ub2pUTtl6xz4dTFz++ji4M=",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := string(relaxedBody([]byte(test.body)))
			if got != test.canonical {
				t.Errorf("relaxedBody(%q) = %q, want %q", test.body, got, test.canonical)
			}
			digest := sha256.Sum256([]byte(got))
			if encoded := base64.StdEncoding.EncodeToString(digest[:]); encoded != test.bodyHash {
				t.Errorf("body hash = %q, want %q", encoded, test.bodyHash)
			}
		})
	}
}

func TestRelaxedHeaderCanonicalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "lowercases and strips around the colon", raw: "SUBJECT :  hello", want: "subject:hello"},
		{name: "collapses internal whitespace", raw: "Subject: a  b\tc", want: "subject:a b c"},
		{name: "unfolds continuations", raw: "Subject: a\r\n\tb\r\n  c", want: "subject:a b c"},
		{name: "drops trailing whitespace", raw: "To: bob@example.com   ", want: "to:bob@example.com"},
		{name: "keeps an empty value", raw: "X-Empty:", want: "x-empty:"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			name, _, _ := strings.Cut(test.raw, ":")
			field := headerField{name: strings.ToLower(strings.TrimSpace(name)), raw: test.raw}
			if got := relaxedHeader(field); got != test.want {
				t.Errorf("relaxedHeader(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Selectors
// -----------------------------------------------------------------------------

func TestExpandDkimSelector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		template  string
		version   int
		algorithm string
		want      string
		wantErr   bool
	}{
		{
			// The two values the captured probe zone file actually carries.
			name: "the probe's rsa selector", template: DefaultSelectorTemplate,
			version: 1, algorithm: DkimAlgorithmRSA, want: "v1-rsa-20260903",
		},
		{
			name: "the probe's ed25519 selector", template: DefaultSelectorTemplate,
			version: 1, algorithm: DkimAlgorithmEd25519, want: "v1-ed25519-20260903",
		},
		{
			name: "an empty template is the default", template: "",
			version: 2, algorithm: DkimAlgorithmRSA, want: "v2-rsa-20260903",
		},
		{
			name: "a version below one is one", template: DefaultSelectorTemplate,
			version: 0, algorithm: DkimAlgorithmRSA, want: "v1-rsa-20260903",
		},
		{
			name: "a literal template has no placeholders", template: "mail",
			version: 1, algorithm: DkimAlgorithmRSA, want: "mail",
		},
		{
			name: "the strftime subset", template: "{date-%Y%m%d%H%M%S}-{algorithm}",
			version: 1, algorithm: DkimAlgorithmRSA, want: "20260903085228-rsa",
		},
		{
			name: "a percent escapes itself", template: "a{date-%%}b",
			version: 1, algorithm: DkimAlgorithmRSA, wantErr: true, // '%' is not a DNS label byte
		},
		{
			name: "an unknown placeholder is refused", template: "v{revision}",
			version: 1, algorithm: DkimAlgorithmRSA, wantErr: true,
		},
		{
			name: "an unterminated placeholder is refused", template: "v{version",
			version: 1, algorithm: DkimAlgorithmRSA, wantErr: true,
		},
		{
			name: "an unknown strftime verb is refused", template: "{date-%Q}",
			version: 1, algorithm: DkimAlgorithmRSA, wantErr: true,
		},
		{
			name: "an unknown algorithm is refused", template: DefaultSelectorTemplate,
			version: 1, algorithm: "Dkim1Sha1", wantErr: true,
		},
		{
			name: "an empty selector is refused", template: "",
			version: 1, algorithm: "", wantErr: true,
		},
		{
			name: "a selector longer than a DNS label is refused", template: strings.Repeat("a", 64),
			version: 1, algorithm: DkimAlgorithmRSA, wantErr: true,
		},
		{
			name: "a leading hyphen is refused", template: "-{algorithm}",
			version: 1, algorithm: DkimAlgorithmRSA, wantErr: true,
		},
		{
			name: "an underscore is refused", template: "v{version}_{algorithm}",
			version: 1, algorithm: DkimAlgorithmRSA, wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ExpandDkimSelector(test.template, test.version, test.algorithm, referenceTime)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ExpandDkimSelector(%q) = %q, want an error", test.template, got)
				}
				if !errors.Is(err, ErrInvalid) {
					t.Errorf("error = %v, want it to wrap ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandDkimSelector(%q): %v", test.template, err)
			}
			if got != test.want {
				t.Errorf("ExpandDkimSelector(%q) = %q, want %q", test.template, got, test.want)
			}
		})
	}
}

// TestExpandDkimSelectorIsUTC proves the selector does not depend on where the
// process runs. A selector that changed with the local zone would be published
// under one name and signed under another for anyone east of UTC.
func TestExpandDkimSelectorIsUTC(t *testing.T) {
	t.Parallel()

	honolulu := time.FixedZone("HST", -10*60*60)
	local := referenceTime.In(honolulu)
	if local.Day() == referenceTime.Day() {
		t.Fatalf("the fixture time must cross a date boundary in the test zone")
	}
	got, err := ExpandDkimSelector(DefaultSelectorTemplate, 1, DkimAlgorithmRSA, local)
	if err != nil {
		t.Fatalf("ExpandDkimSelector: %v", err)
	}
	if got != "v1-rsa-20260903" {
		t.Errorf("selector = %q, want the UTC date v1-rsa-20260903", got)
	}
}

// -----------------------------------------------------------------------------
// Generation
// -----------------------------------------------------------------------------

func TestGenerateDkimKeys(t *testing.T) {
	t.Parallel()

	keys, err := GenerateDkimKeys("d-test", DkimOptions{
		Now:         referenceTime,
		RotateAfter: 90 * 24 * time.Hour,
		RetireAfter: 7 * 24 * time.Hour,
		DeleteAfter: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("GenerateDkimKeys: %v", err)
	}
	if len(keys) != len(RequestedDkimAlgorithms) {
		t.Fatalf("got %d keys, want %d — the facade waits for one per algorithm", len(keys), len(RequestedDkimAlgorithms))
	}

	bySelector := map[string]DkimKey{}
	for _, key := range keys {
		bySelector[key.Selector] = key
	}

	tests := []struct {
		selector  string
		algorithm string
		publicLen int
	}{
		{selector: "v1-ed25519-20260903", algorithm: DkimAlgorithmEd25519, publicLen: ed25519.PublicKeySize},
		{selector: "v1-rsa-20260903", algorithm: DkimAlgorithmRSA},
	}

	for _, test := range tests {
		t.Run(test.selector, func(t *testing.T) {
			key, found := bySelector[test.selector]
			if !found {
				t.Fatalf("no key with selector %q; got %v", test.selector, keys)
			}
			if key.Algorithm != test.algorithm {
				t.Errorf("algorithm = %q, want %q", key.Algorithm, test.algorithm)
			}
			// Active, not pending: BindMailDomain gives ListDkim 20 s to report
			// an active signature per algorithm and shows a DEGRADED binding
			// until it does.
			if !key.Active() {
				t.Errorf("stage = %q, want %q", key.Stage, DkimStageActive)
			}
			if key.ID != DkimID("d-test", test.selector) {
				t.Errorf("id = %q, want the derived id", key.ID)
			}
			if key.Version != 1 {
				t.Errorf("version = %d, want 1", key.Version)
			}
			if !key.CreatedAt.Equal(referenceTime) {
				t.Errorf("createdAt = %v, want %v", key.CreatedAt, referenceTime)
			}
			if !key.RotateAt.Equal(referenceTime.Add(90 * 24 * time.Hour)) {
				t.Errorf("rotateAt = %v, want the creation time plus 90 days", key.RotateAt)
			}
			if !key.RetireAt.Equal(referenceTime.Add(7*24*time.Hour)) || !key.DeleteAt.Equal(referenceTime.Add(30*24*time.Hour)) {
				t.Errorf("retireAt/deleteAt = %v/%v, want +7d/+30d", key.RetireAt, key.DeleteAt)
			}

			public, decodeErr := base64.StdEncoding.DecodeString(key.PublicKey)
			if decodeErr != nil {
				t.Fatalf("the public key is not base64: %v", decodeErr)
			}
			if len(key.PrivateKey) == 0 {
				t.Fatal("the private key is missing")
			}
			if _, parseErr := x509.ParsePKCS8PrivateKey(key.PrivateKey.Bytes()); parseErr != nil {
				t.Fatalf("the private key is not PKCS#8: %v", parseErr)
			}

			if test.algorithm == DkimAlgorithmEd25519 {
				if len(public) != test.publicLen {
					t.Errorf("public key is %d bytes, want the raw %d-byte key", len(public), test.publicLen)
				}
				return
			}
			parsed, parseErr := x509.ParsePKIXPublicKey(public)
			if parseErr != nil {
				t.Fatalf("the RSA public key is not a SubjectPublicKeyInfo: %v", parseErr)
			}
			rsaPublic, ok := parsed.(*rsa.PublicKey)
			if !ok {
				t.Fatalf("the RSA public key decoded to %T", parsed)
			}
			if rsaPublic.N.BitLen() != DkimRSABits {
				t.Errorf("modulus is %d bits, want %d", rsaPublic.N.BitLen(), DkimRSABits)
			}
			if !strings.HasPrefix(key.PublicKey, "MIIBIjANBgkqhkiG9w0BAQEFAAOC") {
				t.Errorf("public key starts %q; the captured fixture's SPKI prefix is MIIBIjANBgkqhkiG9w0BAQEFAAOC", key.PublicKey[:28])
			}
		})
	}
}

// TestGenerateDkimKeysRefusesCollidingSelectors covers the trap in a template
// with no {algorithm}: both keys would derive the same DkimID, the second would
// overwrite the first, and the domain would come up with one algorithm and a
// binding that never leaves DEGRADED.
func TestGenerateDkimKeysRefusesCollidingSelectors(t *testing.T) {
	t.Parallel()

	_, err := GenerateDkimKeys("d-test", DkimOptions{SelectorTemplate: "v{version}", Now: referenceTime})
	if err == nil {
		t.Fatal("GenerateDkimKeys accepted a template that gives both algorithms one selector")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error = %v, want it to wrap ErrInvalid", err)
	}
}

func TestProvisionDkimKeys(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openTestStore(t)
	domain, err := store.CreateDomain(ctx, NewDomain("acme.dev", referenceTime))
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}

	first, err := ProvisionDkimKeys(ctx, store, domain.ID, DkimOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("ProvisionDkimKeys: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("got %d keys, want 2", len(first))
	}

	stored, err := store.ListDkimKeys(ctx, domain.ID)
	if err != nil {
		t.Fatalf("ListDkimKeys: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("the store holds %d keys, want 2", len(stored))
	}
	for _, key := range stored {
		if len(key.PrivateKey) == 0 {
			t.Errorf("selector %q round-tripped without its private key", key.Selector)
		}
		if key.PublicKey == "" {
			t.Errorf("selector %q round-tripped without its public key", key.Selector)
		}
	}

	// A second provision rotates: new version, new selectors, and the keys that
	// are currently signing mail are still there.
	second, err := ProvisionDkimKeys(ctx, store, domain.ID, DkimOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("ProvisionDkimKeys (rotation): %v", err)
	}
	for _, key := range second {
		if key.Version != 2 {
			t.Errorf("rotated key %q is version %d, want 2", key.Selector, key.Version)
		}
		if !strings.HasPrefix(key.Selector, "v2-") {
			t.Errorf("rotated selector = %q, want a v2- selector", key.Selector)
		}
	}
	after, err := store.ListDkimKeys(ctx, domain.ID)
	if err != nil {
		t.Fatalf("ListDkimKeys: %v", err)
	}
	if len(after) != 4 {
		t.Fatalf("the store holds %d keys after a rotation, want 4", len(after))
	}
}

func TestProvisionDkimKeysRefusesAnUnknownDomain(t *testing.T) {
	t.Parallel()

	if _, err := ProvisionDkimKeys(context.Background(), openTestStore(t), "d-missing", DkimOptions{Now: referenceTime}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// openTestStore opens a store under t.TempDir. It is deliberately not shared
// with store_test.go's helpers so this file stands alone.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "maild.db"), WithClock(func() time.Time { return referenceTime }))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("Close: %v", closeErr)
		}
	})
	return store
}

// -----------------------------------------------------------------------------
// Publication
// -----------------------------------------------------------------------------

// TestDkimRecordValue checks the three rules internal/mail's validDkimValue
// enforces. A value that breaks any of them is not published at all, its
// algorithm is dropped from the readiness set, and the binding never completes
// — the failure appears to the operator as "email is not configured".
func TestDkimRecordValue(t *testing.T) {
	t.Parallel()

	keys, err := GenerateDkimKeys("d-test", DkimOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("GenerateDkimKeys: %v", err)
	}

	for _, key := range keys {
		t.Run(key.Selector, func(t *testing.T) {
			value := DkimRecordValue(key)

			var order []string
			tags := map[string]string{}
			for _, part := range strings.Split(value, ";") {
				name, tagValue, found := strings.Cut(strings.TrimSpace(part), "=")
				if !found {
					continue
				}
				order = append(order, strings.TrimSpace(name))
				tags[strings.TrimSpace(name)] = strings.TrimSpace(tagValue)
			}
			if len(order) == 0 || order[0] != "v" {
				t.Fatalf("the first tag is %v, want v — validDkimValue refuses anything else", order)
			}
			if tags["v"] != "DKIM1" {
				t.Errorf("v = %q, want DKIM1", tags["v"])
			}
			// A missing k is refused even though RFC 6376 defaults it to rsa.
			switch tags["k"] {
			case "rsa", "ed25519":
			default:
				t.Errorf("k = %q, want rsa or ed25519", tags["k"])
			}
			if tags["k"] != DkimKeyType[key.Algorithm] {
				t.Errorf("k = %q, want %q for %s", tags["k"], DkimKeyType[key.Algorithm], key.Algorithm)
			}
			if tags["h"] != "sha256" {
				t.Errorf("h = %q, want sha256", tags["h"])
			}
			if _, decodeErr := base64.StdEncoding.DecodeString(tags["p"]); decodeErr != nil || tags["p"] == "" {
				t.Errorf("p = %q, which must be non-empty base64: %v", tags["p"], decodeErr)
			}
			if tags["p"] != key.PublicKey {
				t.Error("the record's p= and the JMAP publicKey property disagree; the facade shows one and publishes the other")
			}
		})
	}
}

func TestDkimRecordValueRefusesAnUnknownAlgorithm(t *testing.T) {
	t.Parallel()

	if value := DkimRecordValue(DkimKey{Selector: "v1-x", Algorithm: "Dkim1Sha1", PublicKey: "AAAA"}); value != "" {
		t.Errorf("DkimRecordValue = %q, want an empty value rather than a record receivers would fail on", value)
	}
}

// TestDkimZoneRecord parses the rendered lines with the zone-file parser this
// module already depends on, which is a real check of the BIND syntax rather
// than a string comparison, and then joins the character-strings back the way
// the facade does.
func TestDkimZoneRecord(t *testing.T) {
	t.Parallel()

	keys, err := GenerateDkimKeys("d-test", DkimOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("GenerateDkimKeys: %v", err)
	}

	for _, key := range keys {
		t.Run(key.Selector, func(t *testing.T) {
			record := DkimZoneRecord(key, "Probe.Test.")
			owner := key.Selector + "._domainkey.probe.test."
			if !strings.HasPrefix(record, owner+" IN TXT ") {
				t.Fatalf("record = %q, want it to start with %q", record, owner+" IN TXT ")
			}

			parsed, parseErr := dns.NewRR(record)
			if parseErr != nil {
				t.Fatalf("the rendered record does not parse as a zone-file line: %v\n%s", parseErr, record)
			}
			txt, ok := parsed.(*dns.TXT)
			if !ok {
				t.Fatalf("the record parsed as %T, want a TXT", parsed)
			}
			if got := strings.Join(txt.Txt, ""); got != DkimRecordValue(key) {
				t.Errorf("joined character-strings = %q, want %q", got, DkimRecordValue(key))
			}
			for index, part := range txt.Txt {
				if len(part) > maxCharacterString {
					t.Errorf("character-string %d is %d bytes, over the %d-byte limit", index, len(part), maxCharacterString)
				}
			}

			// An RSA-2048 key does not fit in one character-string; an Ed25519
			// one does. The probe capture shows exactly this difference.
			multiline := strings.Contains(record, "(\n")
			if key.Algorithm == DkimAlgorithmRSA && !multiline {
				t.Error("the RSA record is a single string, but its value is over 255 bytes")
			}
			if key.Algorithm == DkimAlgorithmEd25519 && multiline {
				t.Error("the Ed25519 record was split, but its value fits in one character-string")
			}
		})
	}
}

// TestDkimZoneRecordsPublishesOnlyActiveKeys is the rule the facade relies on:
// a retired key's record would invite receivers to verify signatures this
// server no longer makes.
func TestDkimZoneRecordsPublishesOnlyActiveKeys(t *testing.T) {
	t.Parallel()

	keys, err := GenerateDkimKeys("d-test", DkimOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("GenerateDkimKeys: %v", err)
	}
	keys[0].Stage = DkimStageRetired

	rendered := DkimZoneRecords(keys, "probe.test")
	if strings.Contains(rendered, keys[0].Selector) {
		t.Errorf("the retired selector %q was published", keys[0].Selector)
	}
	if !strings.Contains(rendered, keys[1].Selector) {
		t.Errorf("the active selector %q was not published", keys[1].Selector)
	}
	if !strings.HasSuffix(rendered, "\n") {
		t.Error("the rendered records are not newline terminated")
	}
}

func TestSplitCharacterStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		limit int
		want  []string
	}{
		{name: "a short value is one string", value: "abc", limit: 4, want: []string{"abc"}},
		{name: "an exact fit is one string", value: "abcd", limit: 4, want: []string{"abcd"}},
		{name: "a long value splits", value: "abcdefghi", limit: 4, want: []string{"abcd", "efgh", "i"}},
		{name: "an empty value is one empty string", value: "", limit: 4, want: []string{""}},
		{name: "a non-positive limit does not split", value: "abcd", limit: 0, want: []string{"abcd"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := splitCharacterStrings(test.value, test.limit)
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Errorf("splitCharacterStrings(%q, %d) = %q, want %q", test.value, test.limit, got, test.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Signing
// -----------------------------------------------------------------------------

const testMessage = "From: Ada Lovelace <ada@acme.dev>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Subject: Testing  DKIM\r\n" +
	"Date: Thu, 03 Sep 2026 08:52:28 +0000\r\n" +
	"Message-ID: <1@acme.dev>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Hello,  world.  \r\n" +
	"\r\n" +
	"This is a test.\r\n"

// TestSignedMessageVerifies is the load-bearing test in this file: it signs with
// each algorithm and then verifies with the independent verifier above, against
// the public key taken out of the TXT record this server publishes. That closes
// the loop the deployment actually depends on — the record and the signature
// agree — and it proves the arithmetic rather than restating the signer.
func TestSignedMessageVerifies(t *testing.T) {
	t.Parallel()

	keys, err := GenerateDkimKeys("d-test", DkimOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("GenerateDkimKeys: %v", err)
	}

	for _, key := range keys {
		t.Run(key.Algorithm, func(t *testing.T) {
			signer, signerErr := NewDkimSigner("acme.dev", key, SignOptions{Now: referenceTime})
			if signerErr != nil {
				t.Fatalf("NewDkimSigner: %v", signerErr)
			}
			signed, signErr := signer.Sign([]byte(testMessage))
			if signErr != nil {
				t.Fatalf("Sign: %v", signErr)
			}

			if !strings.HasPrefix(string(signed), "DKIM-Signature: v=1;") {
				t.Fatalf("the signed message does not start with the signature header:\n%.120s", signed)
			}
			if !strings.HasSuffix(string(signed), testMessage) {
				t.Error("signing changed the message body")
			}
			for _, line := range strings.Split(strings.TrimSuffix(string(signed), "\r\n"), "\r\n") {
				if len(line) > 998 {
					t.Errorf("a line is %d bytes, over the SMTP limit", len(line))
				}
			}

			if verifyErr := verifyDkim(string(signed), DkimRecordValue(key)); verifyErr != nil {
				t.Fatalf("the signature this server produced does not verify: %v", verifyErr)
			}
		})
	}
}

// TestSignedMessageFailsWhenTampered is the other half: a verifier that accepts
// everything would pass the test above.
func TestSignedMessageFailsWhenTampered(t *testing.T) {
	t.Parallel()

	keys, err := GenerateDkimKeys("d-test", DkimOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("GenerateDkimKeys: %v", err)
	}

	tests := []struct {
		name   string
		tamper func(signed string) string
		reason string
	}{
		{
			name:   "a changed body",
			tamper: func(signed string) string { return strings.Replace(signed, "This is a test.", "This is a fake.", 1) },
			reason: "body hash",
		},
		{
			name:   "a body with a line appended",
			tamper: func(signed string) string { return signed + "PS. send money\r\n" },
			reason: "body hash",
		},
		{
			name:   "a rewritten Subject",
			tamper: func(signed string) string { return strings.Replace(signed, "Testing  DKIM", "Your invoice", 1) },
			reason: "signature",
		},
		{
			name:   "a rewritten From",
			tamper: func(signed string) string { return strings.Replace(signed, "ada@acme.dev", "mallory@acme.dev", 1) },
			reason: "signature",
		},
		{
			name: "a flipped byte in b=",
			tamper: func(signed string) string {
				header, rest, _ := strings.Cut(signed, "\r\nFrom:")
				index := strings.LastIndex(header, "b=") + 2
				flipped := []byte(header)
				if flipped[index] == 'A' {
					flipped[index] = 'B'
				} else {
					flipped[index] = 'A'
				}
				return string(flipped) + "\r\nFrom:" + rest
			},
			reason: "signature",
		},
		{
			name: "a signature from another domain's key",
			tamper: func(signed string) string {
				return strings.Replace(signed, "d=acme.dev;", "d=evil.example;", 1)
			},
			reason: "signature",
		},
	}

	for _, key := range keys {
		for _, test := range tests {
			t.Run(key.Algorithm+"/"+test.name, func(t *testing.T) {
				signer, signerErr := NewDkimSigner("acme.dev", key, SignOptions{Now: referenceTime})
				if signerErr != nil {
					t.Fatalf("NewDkimSigner: %v", signerErr)
				}
				signed, signErr := signer.Sign([]byte(testMessage))
				if signErr != nil {
					t.Fatalf("Sign: %v", signErr)
				}
				if verifyErr := verifyDkim(string(signed), DkimRecordValue(key)); verifyErr != nil {
					t.Fatalf("the untampered signature does not verify: %v", verifyErr)
				}
				if verifyErr := verifyDkim(test.tamper(string(signed)), DkimRecordValue(key)); verifyErr == nil {
					t.Fatalf("%s verified; the %s check did not fire", test.name, test.reason)
				}
			})
		}
	}
}

// TestSignatureHeaderContents pins the tags a receiver reads.
func TestSignatureHeaderContents(t *testing.T) {
	t.Parallel()

	key := testKey(t, DkimAlgorithmRSA)
	signer, err := NewDkimSigner("Acme.Dev.", key, SignOptions{Now: referenceTime, Expires: 14 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewDkimSigner: %v", err)
	}
	if signer.Domain() != "acme.dev" {
		t.Errorf("Domain() = %q, want the normalised acme.dev", signer.Domain())
	}
	if signer.Selector() != key.Selector {
		t.Errorf("Selector() = %q, want %q", signer.Selector(), key.Selector)
	}

	header, err := signer.SignatureHeader([]byte(testMessage))
	if err != nil {
		t.Fatalf("SignatureHeader: %v", err)
	}
	_, value, _ := strings.Cut(header, ":")
	tags := parseTagList(value)

	want := map[string]string{
		"v": "1",
		"a": DkimSignatureAlgorithmRSA,
		"c": "relaxed/relaxed",
		"d": "acme.dev",
		"s": key.Selector,
		"t": fmt.Sprintf("%d", referenceTime.Unix()),
		"x": fmt.Sprintf("%d", referenceTime.Add(14*24*time.Hour).Unix()),
		// Only the headers the message actually carries, in the configured
		// order. Naming an absent header in h= is legal but pointless, and
		// naming one twice fails the whole signature.
		"h": "from:subject:date:message-id:to:mime-version:content-type",
	}
	for name, expected := range want {
		if tags[name] != expected {
			t.Errorf("%s= is %q, want %q", name, tags[name], expected)
		}
	}
	if strings.HasSuffix(header, "\r\n") {
		t.Error("SignatureHeader returned a trailing CRLF; the caller adds it")
	}
}

func TestSignerRefusesAMessageWithoutFrom(t *testing.T) {
	t.Parallel()

	signer, err := NewDkimSigner("acme.dev", testKey(t, DkimAlgorithmEd25519), SignOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("NewDkimSigner: %v", err)
	}
	if _, signErr := signer.Sign([]byte("Subject: no sender\r\n\r\nbody\r\n")); !errors.Is(signErr, ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid: RFC 6376 §5.4 makes From mandatory", signErr)
	}
}

// TestSignNormalizesLineEndings covers a message assembled from Go literals: it
// must sign and transmit as CRLF, or the body hash covers text nobody sends.
func TestSignNormalizesLineEndings(t *testing.T) {
	t.Parallel()

	key := testKey(t, DkimAlgorithmEd25519)
	signer, err := NewDkimSigner("acme.dev", key, SignOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("NewDkimSigner: %v", err)
	}
	signed, err := signer.Sign([]byte("From: ada@acme.dev\nSubject: bare newlines\n\nbody\n"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	for index := 0; index < len(signed); index++ {
		if signed[index] == '\n' && (index == 0 || signed[index-1] != '\r') {
			t.Fatalf("a bare LF survived at offset %d", index)
		}
	}
	if verifyErr := verifyDkim(string(signed), DkimRecordValue(key)); verifyErr != nil {
		t.Fatalf("the signature does not verify: %v", verifyErr)
	}
}

// TestSignMessageDoubleSigns is what outbound mail does: an RSA signature,
// which is the only one Gmail and Microsoft verify, plus an Ed25519 one.
func TestSignMessageDoubleSigns(t *testing.T) {
	t.Parallel()

	keys, err := GenerateDkimKeys("d-test", DkimOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("GenerateDkimKeys: %v", err)
	}
	keys = append(keys, DkimKey{Selector: "v0-retired", Algorithm: DkimAlgorithmRSA, Stage: DkimStageRetired})

	signed, err := SignMessage([]byte(testMessage), "acme.dev", keys, SignOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("SignMessage: %v", err)
	}
	if count := strings.Count(string(signed), "DKIM-Signature:"); count != 2 {
		t.Fatalf("got %d signatures, want 2 (the retired key must be skipped)", count)
	}

	// Each signature verifies on its own. SignMessage prepends, so the last
	// key signed is the topmost header, and the verifier reads the topmost one
	// — peel them off in reverse.
	var active []DkimKey
	for _, key := range keys {
		if key.Active() {
			active = append(active, key)
		}
	}
	remaining := string(signed)
	for index := len(active) - 1; index >= 0; index-- {
		key := active[index]
		if !strings.Contains(remaining, "s="+key.Selector+";") {
			t.Fatalf("the topmost signature is not selector %q:\n%.200s", key.Selector, remaining)
		}
		if verifyErr := verifyDkim(remaining, DkimRecordValue(key)); verifyErr != nil {
			t.Fatalf("selector %q does not verify: %v", key.Selector, verifyErr)
		}
		_, rest, found := strings.Cut(remaining, "\r\nDKIM-Signature:")
		if !found {
			break
		}
		remaining = "DKIM-Signature:" + rest
	}
}

func TestSignMessageWithoutActiveKeysIsAPassthrough(t *testing.T) {
	t.Parallel()

	signed, err := SignMessage([]byte(testMessage), "acme.dev", nil, SignOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("SignMessage: %v", err)
	}
	// Unsigned mail is delivered; mail that could not be sent is not.
	if string(signed) != testMessage {
		t.Error("SignMessage changed a message it had no key for")
	}
}

func TestNewDkimSignerRejects(t *testing.T) {
	t.Parallel()

	good := testKey(t, DkimAlgorithmRSA)
	ed := testKey(t, DkimAlgorithmEd25519)

	tests := []struct {
		name   string
		domain string
		key    DkimKey
	}{
		{name: "no domain", domain: "  ", key: good},
		{name: "no selector", domain: "acme.dev", key: DkimKey{Algorithm: DkimAlgorithmRSA, PrivateKey: good.PrivateKey}},
		{name: "unknown algorithm", domain: "acme.dev", key: DkimKey{Selector: "s", Algorithm: "Dkim1Sha1", PrivateKey: good.PrivateKey}},
		{name: "no private key", domain: "acme.dev", key: DkimKey{Selector: "s", Algorithm: DkimAlgorithmRSA}},
		{name: "unparseable private key", domain: "acme.dev", key: DkimKey{Selector: "s", Algorithm: DkimAlgorithmRSA, PrivateKey: Secret("not der")}},
		{name: "algorithm disagrees with the key", domain: "acme.dev", key: DkimKey{Selector: "s", Algorithm: DkimAlgorithmRSA, PrivateKey: ed.PrivateKey}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			signer, err := NewDkimSigner(test.domain, test.key, SignOptions{})
			if err == nil {
				t.Fatalf("NewDkimSigner accepted %s (selector %q)", test.name, signer.Selector())
			}
			// The refusal names the selector, never the key material.
			if len(test.key.PrivateKey) > 0 && strings.Contains(err.Error(), string(test.key.PrivateKey.Bytes())) {
				t.Error("the error text quotes the private key")
			}
		})
	}
}

// TestDkimSecretsNeverPrint is the structural half of the "secrets never leave"
// rule: no formatting verb, and no wire object, can emit a private key.
func TestDkimSecretsNeverPrint(t *testing.T) {
	t.Parallel()

	key := testKey(t, DkimAlgorithmEd25519)
	needle := base64.StdEncoding.EncodeToString(key.PrivateKey.Bytes())

	for _, format := range []string{"%v", "%s", "%#v", "%+v"} {
		rendered := fmt.Sprintf(format, key)
		if strings.Contains(rendered, needle) || strings.Contains(rendered, string(key.PrivateKey.Bytes())) {
			t.Errorf("fmt.Sprintf(%q, key) leaked the private key", format)
		}
		if !strings.Contains(rendered, "[redacted]") {
			t.Errorf("fmt.Sprintf(%q, key) = %q, want the redaction", format, rendered)
		}
	}

	encoded, err := json.Marshal(key.Wire())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), needle) {
		t.Error("the JMAP x:DkimSignature object carries the private key")
	}
	if !strings.Contains(string(encoded), key.PublicKey) {
		t.Error("the JMAP x:DkimSignature object does not carry the public key")
	}
}

// testKey generates one key of the given algorithm.
func testKey(t *testing.T, algorithm string) DkimKey {
	t.Helper()
	key, err := GenerateDkimKey("d-test", algorithm, DkimOptions{Now: referenceTime})
	if err != nil {
		t.Fatalf("GenerateDkimKey(%s): %v", algorithm, err)
	}
	return key
}
