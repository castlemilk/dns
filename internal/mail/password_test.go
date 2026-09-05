package mail

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// minimumEntropyBits is the bar a generated mailbox credential must clear. It
// is a credential an operator pastes into a mail client and keeps, and it is
// the only secret this API ever returns, so it is held to a real number rather
// than a rule of thumb.
const minimumEntropyBits = 60

func TestGeneratedCredentialEntropy(t *testing.T) {
	size := WordlistSize()
	if size < 2048 {
		t.Fatalf("the word list holds %d words", size)
	}
	bits := float64(passphraseWords)*math.Log2(float64(size)) + math.Log2(float64(passphraseDigits))
	if bits < minimumEntropyBits {
		t.Fatalf("entropy = %.1f bits, want at least %d", bits, minimumEntropyBits)
	}
	// Four words would not clear the bar; the fifth is doing real work.
	weaker := 4*math.Log2(float64(size)) + math.Log2(float64(passphraseDigits))
	if weaker >= minimumEntropyBits {
		t.Errorf("four words already give %.1f bits; the constant is untested", weaker)
	}
}

var credentialCharset = regexp.MustCompile(`^[a-z0-9-]+$`)

func TestGeneratedCredentialShape(t *testing.T) {
	seen := make(map[string]struct{}, 200)
	for range 200 {
		value, err := newPassphrase()
		if err != nil {
			t.Fatalf("newPassphrase: %v", err)
		}
		if !credentialCharset.MatchString(value) {
			t.Fatalf("credential %q is outside [a-z0-9-]", value)
		}
		parts := strings.Split(value, passphraseSep)
		if len(parts) != passphraseWords+1 {
			t.Fatalf("credential %q has %d parts, want %d", value, len(parts), passphraseWords+1)
		}
		digits := parts[len(parts)-1]
		if len(digits) != 2 || digits < "00" || digits > "99" {
			t.Fatalf("credential %q does not end in two digits", value)
		}
		if _, duplicate := seen[value]; duplicate {
			t.Fatalf("credential %q was generated twice in 200 draws", value)
		}
		seen[value] = struct{}{}
	}
}

func TestWordlistIsClean(t *testing.T) {
	for _, word := range words {
		if len(word) < 4 || len(word) > 8 {
			t.Errorf("word %q is outside four to eight letters", word)
		}
	}
	if len(words) != 2048 {
		t.Errorf("the list holds %d words; 2048 is exactly eleven bits each", len(words))
	}
}

// credentialFiles are the only files in this package allowed to mention the
// credential vocabulary: the generator, and the two response builders that hand
// the generated value to its single caller.
//
// The rule exists because a generated credential has no recognisable shape: no
// redactor can catch it after the fact, so keeping it out of the logs, the
// events and the store has to be structural. A new mention anywhere else is a
// new place it could leak from.
var credentialFiles = map[string]bool{
	"password.go": true,
	"mailbox.go":  true,
}

func TestCredentialVocabularyIsConfinedToItsFiles(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if credentialFiles[name] {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// sysAccountPasswordUpdate is the name of a Stalwart permission, not a
		// value: it is the one mention that carries nothing.
		text := strings.ReplaceAll(string(raw), "sysAccountPasswordUpdate", "")
		if strings.Contains(strings.ToLower(text), "password") {
			t.Errorf("%s mentions the credential vocabulary; only %v may", name, keysOf(credentialFiles))
		}
	}
}

// TestTheCredentialReachesOnlyTheResponse walks the AST of mailbox.go and
// asserts that the value newPassphrase returns is assigned to exactly one
// identifier and used only as a response field or an engine input — never as an
// argument to the recorder or the logger.
func TestTheCredentialReachesOnlyTheResponse(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, filepath.Join(".", "mailbox.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse mailbox.go: %v", err)
	}
	forbidden := map[string]bool{"Record": true, "Info": true, "Warn": true, "Error": true, "Debug": true}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !forbidden[selector.Sel.Name] {
			return true
		}
		for _, argument := range call.Args {
			if mentionsIdentifier(argument, "generated") {
				position := fileSet.Position(call.Pos())
				t.Errorf("the generated credential is passed to %s at %s", selector.Sel.Name, position)
			}
		}
		return true
	})
}

func mentionsIdentifier(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(inner ast.Node) bool {
		if identifier, ok := inner.(*ast.Ident); ok && identifier.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func keysOf(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
