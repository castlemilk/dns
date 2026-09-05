package enginedns_test

import (
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/enginedns"
)

func TestTxtText(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("p", 300)
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "unquoted", input: "v=spf1 mx -all", want: "v=spf1 mx -all"},
		{name: "single string", input: `"v=spf1 mx -all"`, want: "v=spf1 mx -all"},
		{name: "two strings", input: `"a" "bc"`, want: "abc"},
		{name: "two strings without a space", input: `"a""bc"`, want: "abc"},
		{name: "escaped quote", input: `"say \"hi\""`, want: `say "hi"`},
		{name: "leading whitespace", input: "  \"a\" \"b\"  ", want: "ab"},
		{name: "unterminated", input: `"abc`, want: "abc"},
		{name: "split long key", input: `"` + long[:200] + `" "` + long[200:] + `"`, want: long},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := enginedns.TxtText(test.input); got != test.want {
				t.Errorf("TxtText(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestTxtEqual(t *testing.T) {
	t.Parallel()

	if !enginedns.TxtEqual(`"abc"`, `"a" "bc"`) {
		t.Error(`TxtEqual("abc", "a" "bc") = false`)
	}
	if enginedns.TxtEqual(`"a" "b"`, `"ab" "c"`) {
		t.Error("TxtEqual reported two different texts as equal")
	}
	// Byte-equal but differently split values are the same record on the wire;
	// two separate character-strings are not the same as their concatenation
	// for the store's duplicate rule, which is why this comparison exists.
	if !enginedns.TxtEqual("v=spf1 mx -all", `"v=spf1 mx -all"`) {
		t.Error("TxtEqual did not accept an unquoted value")
	}
}
