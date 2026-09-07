package maild

import (
	"errors"
	"fmt"
	"strings"
)

// -----------------------------------------------------------------------------
// Address validation
//
// One parser for every point an address enters this server: SMTP MAIL FROM and
// RCPT TO, JMAP account, alias and list creation, and forwarder targets. There
// is exactly one of these on purpose. An address that is validated in two places
// by two rules is an address that is validated by the weaker rule, and the weak
// rule here has a name: header injection. A bare CR or LF inside a list
// recipient becomes a header of the attacker's choosing in delivered mail,
// because a recipient is written into "Delivered-To:" and into the envelope of
// every fan-out copy.
//
// The rule is deliberately narrower than RFC 5322 permits:
//
//   - every octet must be printable US-ASCII (0x21..0x7E). That single check
//     refuses NUL, CR, LF, TAB, DEL, every other control character and every
//     non-ASCII byte, so a UTF-8 confusable ("аdmin" with a Cyrillic а) can
//     never be mistaken for the account it imitates. SMTPUTF8 is not offered by
//     this server, so accepting non-ASCII would only create addresses that
//     could never be used;
//   - the local part is a dot-atom. Quoted strings ("a b"@x) are legal RFC 5322
//     and are refused: nothing in this platform can create one, and supporting
//     them means carrying an escaping bug forever;
//   - the domain is LDH labels. Address literals (a@[192.0.2.1]) are refused
//     with everything else that is not a label, because the brackets are not
//     printable-ASCII-and-legal here.
//
// Anything that is not a single addr-spec — a display name, angle brackets, a
// comma-separated pair, a second "@" — fails one of those checks and is refused
// as a whole. There is no "best effort" branch: the caller gets an address it
// can key on, or an error.
// -----------------------------------------------------------------------------

// Address is a validated, normalised addr-spec. The zero value is invalid and
// only ParseAddress produces a non-zero one, so a value of this type having
// reached a delivery path is itself the proof that it was checked.
//
// Both parts are lowercased. RFC 5321 §2.4 reserves the case of a local part to
// the receiving host, and this host folds it: the store already keys accounts on
// NormalizeLocalPart, so "Ada@x" and "ada@x" are one mailbox and must produce
// one key.
type Address struct {
	Local  string
	Domain string
}

// Address length limits, in octets.
const (
	// MaxAddressBytes is RFC 5321 §4.5.3.1.3: a path may be 256 octets
	// including the angle brackets, which leaves 254 for the address itself.
	MaxAddressBytes = 254
	// MaxLocalPartBytes is RFC 5321 §4.5.3.1.1.
	MaxLocalPartBytes = 64
	// MaxDomainBytes is RFC 5321 §4.5.3.1.2.
	MaxDomainBytes = 255
	// MaxDomainLabelBytes is RFC 1035 §2.3.4.
	MaxDomainLabelBytes = 63
)

// ErrInvalidAddress is the sentinel every refusal below wraps. Callers test it
// with errors.Is; the wrapped text names the rule that was broken and never
// quotes the input, because the input is exactly the untrusted string that
// wanted to reach a log line or an SMTP reply in the first place.
var ErrInvalidAddress = errors.New("maild: that is not a valid mailbox address")

// The refusal reasons. They are package-level values rather than formatted at
// the point of failure so that the set is closed and greppable, and so that
// no call site can accidentally interpolate the offending input.
var (
	errAddressEmpty      = reasonf("it is empty")
	errAddressTooLong    = reasonf("it is longer than %d octets", MaxAddressBytes)
	errAddressNotASCII   = reasonf("it contains a character that is not printable US-ASCII")
	errAddressNoAt       = reasonf("it does not have exactly one @")
	errLocalEmpty        = reasonf("the part before the @ is empty")
	errLocalTooLong      = reasonf("the part before the @ is longer than %d octets", MaxLocalPartBytes)
	errLocalDot          = reasonf("the part before the @ has an empty dot-separated section")
	errLocalCharacter    = reasonf("the part before the @ has a character that is not allowed")
	errDomainEmpty       = reasonf("the part after the @ is empty")
	errDomainTooLong     = reasonf("the part after the @ is longer than %d octets", MaxDomainBytes)
	errDomainLabelEmpty  = reasonf("the domain has an empty label")
	errDomainLabelLong   = reasonf("the domain has a label longer than %d octets", MaxDomainLabelBytes)
	errDomainLabelHyphen = reasonf("a domain label starts or ends with a hyphen")
	errDomainCharacter   = reasonf("the domain has a character that is not a letter, digit or hyphen")
)

func reasonf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidAddress, fmt.Sprintf(format, args...))
}

// String is the canonical form the store keys on: "local@domain", lowercased.
// The zero Address renders as "" rather than "@" so that a forgotten error
// check produces an obviously empty value instead of a plausible-looking one.
func (a Address) String() string {
	if a.IsZero() {
		return ""
	}
	return a.Local + "@" + a.Domain
}

// IsZero reports whether this is the zero Address, which ParseAddress never
// returns alongside a nil error.
func (a Address) IsZero() bool { return a.Local == "" || a.Domain == "" }

// ParseAddress validates one addr-spec and returns its canonical form.
//
// Leading and trailing ASCII spaces are stripped, because an address arriving
// in JSON commonly carries them and stripping a space cannot hide anything: a
// tab, CR, LF or NUL anywhere — including at the ends — is still a refusal.
// Do not TrimSpace the input before calling. strings.TrimSpace would eat a
// trailing CR, which is the injection this function exists to catch.
//
// The empty string is refused. SMTP's null sender, MAIL FROM:<>, is a valid
// envelope and not a valid address; the caller must recognise it before asking.
func ParseAddress(raw string) (Address, error) {
	address := strings.Trim(raw, " ")
	switch {
	case address == "":
		return Address{}, errAddressEmpty
	case len(address) > MaxAddressBytes:
		return Address{}, errAddressTooLong
	}
	// Byte-wise, not rune-wise: a multi-byte rune has no byte in this range, so
	// scanning octets refuses non-ASCII without decoding anything, and an
	// invalid UTF-8 sequence cannot slip through as U+FFFD.
	for i := 0; i < len(address); i++ {
		if address[i] < 0x21 || address[i] > 0x7e {
			return Address{}, errAddressNotASCII
		}
	}
	local, domain, found := strings.Cut(address, "@")
	if !found || strings.Contains(domain, "@") {
		return Address{}, errAddressNoAt
	}
	if err := validLocalPart(local); err != nil {
		return Address{}, err
	}
	if err := validDomainName(domain); err != nil {
		return Address{}, err
	}
	return Address{Local: strings.ToLower(local), Domain: strings.ToLower(domain)}, nil
}

// NormalizeAddress is ParseAddress for the callers that only want the canonical
// string — a store key, a Delivered-To value, a comparison.
func NormalizeAddress(raw string) (string, error) {
	address, err := ParseAddress(raw)
	if err != nil {
		return "", err
	}
	return address.String(), nil
}

// ValidAddress reports whether raw is acceptable. It is ParseAddress for a
// caller that has nothing to say about which rule was broken; anything that
// reports the failure to a human should use ParseAddress and print the error.
func ValidAddress(raw string) bool {
	_, err := ParseAddress(raw)
	return err == nil
}

// ParseDomainName validates a bare domain — the name of a hosted domain, or the
// right-hand side on its own — under exactly the rule ParseAddress applies to
// the part after the @, and returns it lowercased.
//
// A trailing root dot is refused rather than stripped. NormalizeDomainName
// strips one, which makes "acme.dev." and "acme.dev" the same stored key; this
// function is the gate in front of that, and a gate that silently repairs its
// input teaches callers to send anything.
func ParseDomainName(raw string) (string, error) {
	domain := strings.Trim(raw, " ")
	if domain == "" {
		return "", errDomainEmpty
	}
	for i := 0; i < len(domain); i++ {
		if domain[i] < 0x21 || domain[i] > 0x7e {
			return "", errAddressNotASCII
		}
	}
	if err := validDomainName(domain); err != nil {
		return "", err
	}
	return strings.ToLower(domain), nil
}

// validLocalPart applies RFC 5322 §3.2.3 dot-atom-text. Every octet is already
// known to be printable ASCII.
func validLocalPart(local string) error {
	switch {
	case local == "":
		return errLocalEmpty
	case len(local) > MaxLocalPartBytes:
		return errLocalTooLong
	}
	for _, atom := range strings.Split(local, ".") {
		if atom == "" {
			// A leading, trailing or doubled dot. RFC 5322 forbids all three,
			// and a trailing dot in particular is how ".ada." and "ada" would
			// otherwise become two spellings of one mailbox.
			return errLocalDot
		}
		for i := 0; i < len(atom); i++ {
			if !isAtext(atom[i]) {
				return errLocalCharacter
			}
		}
	}
	return nil
}

// validDomainName applies RFC 1035 preferred syntax, relaxed only in that a
// label may begin with a digit (RFC 1123 §2.1). Every octet is already known to
// be printable ASCII.
//
// A single label is accepted: "root@localhost" is syntactically an address, and
// whether a name resolves is DNS's answer to give, not a parser's.
func validDomainName(domain string) error {
	switch {
	case domain == "":
		return errDomainEmpty
	case len(domain) > MaxDomainBytes:
		return errDomainTooLong
	}
	for _, label := range strings.Split(domain, ".") {
		switch {
		case label == "":
			// A leading dot, a doubled dot, or the root dot of an FQDN.
			return errDomainLabelEmpty
		case len(label) > MaxDomainLabelBytes:
			return errDomainLabelLong
		case label[0] == '-' || label[len(label)-1] == '-':
			return errDomainLabelHyphen
		}
		for i := 0; i < len(label); i++ {
			if !isLDH(label[i]) {
				return errDomainCharacter
			}
		}
	}
	return nil
}

// isAtext is RFC 5322 §3.2.3 atext.
func isAtext(c byte) bool {
	if isAlphanumeric(c) {
		return true
	}
	return strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0
}

// isLDH is the letter-digit-hyphen set of RFC 1035 §2.3.1.
func isLDH(c byte) bool { return isAlphanumeric(c) || c == '-' }

func isAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
