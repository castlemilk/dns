package mail

import (
	"crypto/rand"
	_ "embed"
	"fmt"
	"math/big"
	"strings"
)

// wordlist is the embedded passphrase vocabulary: 2048 common English words of
// four to eight lowercase letters, whitespace-separated. 2048 is exactly 11
// bits per word, so the entropy calculation in the test is exact.
//
//go:embed wordlist.txt
var wordlist string

// words is the parsed list. The package refuses to load with a list that is
// short, duplicated or contains anything outside [a-z], because every one of
// those silently weakens the generated credential.
var words = mustParseWordlist(wordlist)

// Passphrase shape: five words joined by "-", then "-" and two decimal digits.
//
//	5 × log2(2048) + log2(100) = 55 + 6.64 ≈ 61.6 bits
//
// Four words would be ≈ 50.6 bits, which is under the 60-bit bar this product
// sets for a credential an operator pastes into a mail client and keeps. The
// trailing digits also satisfy the "digit required" rule of common password
// policies without asking the reader to remember where a symbol went.
const (
	passphraseWords  = 5
	passphraseDigits = 100
	passphraseSep    = "-"
)

// WordlistSize is the number of words the generator draws from. The entropy
// test computes log2(WordlistSize^5 × 100) from it rather than from a constant,
// so shrinking the list fails the test instead of quietly weakening the result.
func WordlistSize() int { return len(words) }

// newPassphrase returns one generated credential. Every draw comes from
// crypto/rand; there is no fallback to a weaker source, because a credential
// that is silently predictable is worse than an error the caller reports.
//
// The result is returned to exactly one caller, put into exactly one response
// field, and never stored, logged, recorded as an activity detail or used as a
// metric attribute. It has no recognisable shape, so no redactor could catch it
// after the fact — keeping it out is structural, not filtered.
func newPassphrase() (string, error) {
	parts := make([]string, 0, passphraseWords+1)
	for range passphraseWords {
		index, err := randomIndex(len(words))
		if err != nil {
			return "", err
		}
		parts = append(parts, words[index])
	}
	digits, err := randomIndex(passphraseDigits)
	if err != nil {
		return "", err
	}
	parts = append(parts, fmt.Sprintf("%02d", digits))
	return strings.Join(parts, passphraseSep), nil
}

func randomIndex(n int) (int, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, fmt.Errorf("generate a mailbox credential: %w", err)
	}
	return int(value.Int64()), nil
}

func mustParseWordlist(raw string) []string {
	parsed := strings.Fields(raw)
	if len(parsed) < 2048 {
		panic(fmt.Sprintf("mail: the embedded word list has %d words, at least 2048 are required", len(parsed)))
	}
	seen := make(map[string]struct{}, len(parsed))
	for _, word := range parsed {
		if len(word) < 4 {
			panic(fmt.Sprintf("mail: the embedded word list contains a word shorter than four letters: %q", word))
		}
		for _, char := range word {
			if char < 'a' || char > 'z' {
				panic(fmt.Sprintf("mail: the embedded word list contains a non-letter: %q", word))
			}
		}
		if _, duplicate := seen[word]; duplicate {
			panic(fmt.Sprintf("mail: the embedded word list repeats %q", word))
		}
		seen[word] = struct{}{}
	}
	return parsed
}
