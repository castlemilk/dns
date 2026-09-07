package brand_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/brand"
)

// envFunc builds the getenv Detect takes from a literal environment, so no test
// mutates the process environment.
func envFunc(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// characterDevice opens /dev/null, which is a character device and therefore
// indistinguishable from a terminal to Detect. It stands in for a TTY, which a
// test cannot otherwise obtain.
func characterDevice(t *testing.T) *os.File {
	t.Helper()
	file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close %s: %v", os.DevNull, err)
		}
	})
	return file
}

// regularFile is the shape a shell redirect produces: an *os.File that is not a
// character device.
func regularFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return file
}

func TestBannerPlainHasNoEscape(t *testing.T) {
	t.Parallel()

	for _, text := range []string{brand.Banner(brand.ModePlain), brand.Mark(brand.ModePlain)} {
		if strings.ContainsRune(text, 0x1b) {
			t.Errorf("plain variant contains an ESC byte: %q", text)
		}
		for _, char := range text {
			if char != '\n' && (char < 0x20 || char > 0x7e) {
				t.Errorf("plain variant contains the non-ASCII rune %q in %q", char, text)
			}
		}
	}
}

func TestBannerColourVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode brand.Mode
		want string
	}{
		{name: "truecolor uses 24-bit primary", mode: brand.ModeTrueColor, want: "\x1b[38;2;91;156;255m"},
		{name: "256 uses the cube approximation", mode: brand.Mode256, want: "\x1b[38;5;75m"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			banner := brand.Banner(test.mode)
			if !strings.Contains(banner, test.want) {
				t.Errorf("banner in %s mode does not use %q", test.mode, test.want)
			}
			if !strings.HasSuffix(banner, "\n") {
				t.Error("banner is not newline-terminated")
			}
			for _, line := range strings.Split(banner, "\n") {
				if strings.HasSuffix(line, " ") {
					t.Errorf("banner line has trailing whitespace: %q", line)
				}
			}
		})
	}
}

func TestBannerShapeIsStableAcrossModes(t *testing.T) {
	t.Parallel()

	// Strip every escape sequence and swap the ink character back: the three
	// variants must draw the same wordmark, not three different pictures.
	plain := brand.Banner(brand.ModePlain)
	for _, mode := range []brand.Mode{brand.Mode256, brand.ModeTrueColor} {
		stripped := strings.ReplaceAll(stripEscapes(brand.Banner(mode)), "█", "#")
		if stripped != plain {
			t.Errorf("%s mode draws a different shape:\n%s\nwant:\n%s", mode, stripped, plain)
		}
	}
}

func stripEscapes(text string) string {
	var out strings.Builder
	for index := 0; index < len(text); index++ {
		if text[index] != 0x1b {
			out.WriteByte(text[index])
			continue
		}
		for index < len(text) && text[index] != 'm' {
			index++
		}
	}
	return out.String()
}

func TestDetect(t *testing.T) {
	t.Parallel()

	colour := map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"}

	tests := []struct {
		name   string
		writer func(*testing.T) any
		env    map[string]string
		want   brand.Mode
	}{
		{
			name:   "terminal with COLORTERM truecolor",
			writer: func(t *testing.T) any { return characterDevice(t) },
			env:    colour,
			want:   brand.ModeTrueColor,
		},
		{
			name:   "terminal with COLORTERM 24bit",
			writer: func(t *testing.T) any { return characterDevice(t) },
			env:    map[string]string{"TERM": "xterm-256color", "COLORTERM": "24bit"},
			want:   brand.ModeTrueColor,
		},
		{
			name:   "terminal without COLORTERM falls back to 256",
			writer: func(t *testing.T) any { return characterDevice(t) },
			env:    map[string]string{"TERM": "xterm-256color"},
			want:   brand.Mode256,
		},
		{
			name:   "NO_COLOR wins over a colour terminal",
			writer: func(t *testing.T) any { return characterDevice(t) },
			env:    map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "NO_COLOR": "1"},
			want:   brand.ModePlain,
		},
		{
			name:   "TERM=dumb forces plain",
			writer: func(t *testing.T) any { return characterDevice(t) },
			env:    map[string]string{"TERM": "dumb", "COLORTERM": "truecolor"},
			want:   brand.ModePlain,
		},
		{
			name:   "empty TERM forces plain",
			writer: func(t *testing.T) any { return characterDevice(t) },
			env:    map[string]string{"COLORTERM": "truecolor"},
			want:   brand.ModePlain,
		},
		{
			name:   "a buffer is never a terminal",
			writer: func(*testing.T) any { return &bytes.Buffer{} },
			env:    colour,
			want:   brand.ModePlain,
		},
		{
			name:   "a redirected file is never a terminal",
			writer: func(t *testing.T) any { return regularFile(t) },
			env:    colour,
			want:   brand.ModePlain,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			writer, ok := test.writer(t).(interface{ Write([]byte) (int, error) })
			if !ok {
				t.Fatal("writer is not an io.Writer")
			}
			if got := brand.Detect(writer, envFunc(test.env)); got != test.want {
				t.Errorf("Detect = %s, want %s", got, test.want)
			}
		})
	}
}

func TestWriteBannerAndMarkNeverColourANonTTY(t *testing.T) {
	t.Parallel()

	// The strongest form of the promise: with an environment that advertises
	// 24-bit colour, a non-TTY writer still receives not one escape byte.
	env := envFunc(map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"})

	var buffer bytes.Buffer
	if err := brand.WriteBanner(&buffer, env); err != nil {
		t.Fatalf("WriteBanner: %v", err)
	}
	if err := brand.WriteMark(&buffer, env); err != nil {
		t.Fatalf("WriteMark: %v", err)
	}
	if bytes.ContainsRune(buffer.Bytes(), 0x1b) {
		t.Errorf("a non-TTY writer received colour: %q", buffer.String())
	}
	if !strings.Contains(buffer.String(), brand.Wordmark) {
		t.Error("the one-line mark did not spell the wordmark")
	}
}

func TestPaintAndVoices(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		paint func() string
		want  string
	}{
		{name: "plain is untouched", paint: func() string { return brand.Paint(brand.ModePlain, brand.Primary, "hi") }, want: "hi"},
		{name: "empty text is untouched", paint: func() string { return brand.Paint(brand.ModeTrueColor, brand.Primary, "") }, want: ""},
		{name: "accent", paint: func() string { return brand.Accent(brand.Mode256, "hi") }, want: "\x1b[38;5;75mhi\x1b[0m"},
		{name: "ok", paint: func() string { return brand.Ok(brand.Mode256, "hi") }, want: "\x1b[38;5;79mhi\x1b[0m"},
		{name: "warn", paint: func() string { return brand.Warn(brand.Mode256, "hi") }, want: "\x1b[38;5;214mhi\x1b[0m"},
		{
			name:  "truecolor success",
			paint: func() string { return brand.Ok(brand.ModeTrueColor, "hi") },
			want:  "\x1b[38;2;74;222;128mhi\x1b[0m",
		},
		{
			name:  "truecolor warning",
			paint: func() string { return brand.Warn(brand.ModeTrueColor, "hi") },
			want:  "\x1b[38;2;251;191;36mhi\x1b[0m",
		},
		{
			name:  "truecolor foreground",
			paint: func() string { return brand.Paint(brand.ModeTrueColor, brand.Foreground, "hi") },
			want:  "\x1b[38;2;232;234;237mhi\x1b[0m",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := test.paint(); got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

// Every letter of the wordmark must have a glyph. buildWordGlyph silently
// skips a rune it has no shape for, so a wordmark change that forgot a letter
// would ship a banner spelling something else entirely.
func TestEveryWordmarkLetterHasAGlyph(t *testing.T) {
	for _, letter := range brand.Wordmark {
		if _, ok := brand.GlyphFor(letter); !ok {
			t.Errorf("wordmark letter %q has no glyph, so the banner would silently omit it", letter)
		}
	}
}

// The banner has to fit a plain 80-column terminal, mark and all.
func TestBannerFitsEightyColumns(t *testing.T) {
	widest := 0
	for _, line := range strings.Split(brand.Banner(brand.ModePlain), "\n") {
		if n := len([]rune(line)); n > widest {
			widest = n
		}
	}
	if widest == 0 {
		t.Fatal("banner rendered no content")
	}
	if widest > 80 {
		t.Errorf("banner is %d columns wide; it must fit an 80-column terminal", widest)
	}
	t.Logf("banner is %d columns wide", widest)
}
