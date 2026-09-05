// Package brand renders the product's identity in a terminal: the "simple"
// wordmark as an ASCII/ANSI banner, and a compact one-line mark for everything
// else.
//
// The colours are the product's own, taken from apps/web: primary #5b9cff,
// foreground #e8eaed, success #4ade80 and warning #fbbf24. Three variants
// exist — 24-bit, the 256-colour approximation, and plain ASCII — and the
// variant is chosen from the terminal's capability, never from a flag the
// caller has to remember. Plain is the safe default: NO_COLOR, a writer that
// is not a character device, an empty TERM and TERM=dumb all force it, so an
// escape byte can never reach a pipe, a file or a CI log.
package brand

import (
	"io"
	"io/fs"
	"os"
	"strings"
)

// The brand's hexadecimal colours, repeated here so a reader can check them
// against apps/web/app/globals.css without decoding the Color values below.
const (
	HexPrimary    = "#5b9cff"
	HexForeground = "#e8eaed"
	HexSuccess    = "#4ade80"
	HexWarning    = "#fbbf24"
)

// Wordmark is the product name, lowercase, as the banner spells it.
const Wordmark = "simple"

// Mode is the colour depth a writer can take.
type Mode int

const (
	// ModePlain emits no escape bytes at all.
	ModePlain Mode = iota
	// Mode256 emits the xterm 256-colour approximation of the brand palette.
	Mode256
	// ModeTrueColor emits 24-bit colour, matching apps/web exactly.
	ModeTrueColor
)

// String names the mode for diagnostics.
func (m Mode) String() string {
	switch m {
	case ModePlain:
		return "plain"
	case Mode256:
		return "256"
	case ModeTrueColor:
		return "truecolor"
	default:
		return "plain"
	}
}

// Color is one brand colour: its exact 24-bit value and the closest entry in
// the xterm 256-colour cube, so a single value serves both colour modes.
type Color struct {
	R, G, B  uint8
	Index256 uint8
}

// The palette. Index256 values are the nearest xterm cube or greyscale entry
// to the hex value beside them.
var (
	Primary    = Color{R: 0x5b, G: 0x9c, B: 0xff, Index256: 75}
	Foreground = Color{R: 0xe8, G: 0xea, B: 0xed, Index256: 255}
	Success    = Color{R: 0x4a, G: 0xde, B: 0x80, Index256: 79}
	Warning    = Color{R: 0xfb, G: 0xbf, B: 0x24, Index256: 214}
)

const reset = "\x1b[0m"

// Detect reports the colour depth w can take. getenv may be nil, in which case
// the process environment is read.
//
// NO_COLOR (set to anything non-empty), TERM=dumb, an empty TERM and a writer
// that is not a character device each force ModePlain. Otherwise COLORTERM
// decides between 24-bit and the 256-colour approximation; a terminal that
// advertises neither gets the 256-colour variant, which every colour-capable
// terminal in use understands.
func Detect(w io.Writer, getenv func(string) string) Mode {
	if getenv == nil {
		getenv = os.Getenv
	}
	if strings.TrimSpace(getenv("NO_COLOR")) != "" {
		return ModePlain
	}
	term := strings.ToLower(strings.TrimSpace(getenv("TERM")))
	if term == "" || term == "dumb" {
		return ModePlain
	}
	if !isCharacterDevice(w) {
		return ModePlain
	}
	colorterm := strings.ToLower(strings.TrimSpace(getenv("COLORTERM")))
	if colorterm == "truecolor" || colorterm == "24bit" ||
		strings.Contains(term, "truecolor") || strings.Contains(term, "direct") {
		return ModeTrueColor
	}
	return Mode256
}

// isCharacterDevice reports whether w is an *os.File attached to a terminal.
// The standard library exposes no isatty, so the character-device bit stands in
// for one: it is true for a terminal and false for the pipe, socket or regular
// file a redirect produces, which is the distinction that matters here.
func isCharacterDevice(w io.Writer) bool {
	stat, ok := w.(interface{ Stat() (fs.FileInfo, error) })
	if !ok {
		return false
	}
	info, err := stat.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&fs.ModeCharDevice != 0
}

// Paint wraps text in color for mode. In ModePlain, and for empty text, it
// returns text unchanged, so a caller never has to branch on the mode itself.
func Paint(mode Mode, color Color, text string) string {
	if text == "" {
		return text
	}
	switch mode {
	case ModeTrueColor:
		return "\x1b[38;2;" + decimal(color.R) + ";" + decimal(color.G) + ";" + decimal(color.B) + "m" + text + reset
	case Mode256:
		return "\x1b[38;5;" + decimal(color.Index256) + "m" + text + reset
	case ModePlain:
		return text
	default:
		return text
	}
}

// Accent, Ok and Warn are the three named voices the CLI speaks in, so callers
// never reach for a raw Color.
func Accent(mode Mode, text string) string { return Paint(mode, Primary, text) }
func Ok(mode Mode, text string) string     { return Paint(mode, Success, text) }
func Warn(mode Mode, text string) string   { return Paint(mode, Warning, text) }

// decimal renders a byte without fmt, which keeps Paint allocation-cheap.
func decimal(value uint8) string {
	if value < 10 {
		return string([]byte{'0' + value})
	}
	if value < 100 {
		return string([]byte{'0' + value/10, '0' + value%10})
	}
	return string([]byte{'0' + value/100, '0' + (value/10)%10, '0' + value%10})
}

// The banner geometry. Each glyph is five rows of '#' (ink) and ' ' (paper);
// the shapes are identical in every mode, and only the ink character and the
// colour change. The mark is the brand's rounded square, drawn to the left of
// the wordmark exactly as apps/web draws it.
const bannerRows = 5

var (
	markGlyph = [bannerRows]string{
		" ## ",
		"####",
		"####",
		"####",
		" ## ",
	}
	letterGlyphs = map[rune][bannerRows]string{
		's': {"####", "#   ", "####", "   #", "####"},
		'i': {"#", " ", "#", "#", "#"},
		'm': {"#####", "# # #", "# # #", "# # #", "# # #"},
		'p': {"####", "#  #", "####", "#   ", "#   "},
		'l': {"#", "#", "#", "#", "#"},
		'e': {"####", "#  #", "####", "#   ", "####"},
	}
)

// wordGlyph is the five rows of the wordmark, two columns of paper between
// letters so that neighbouring stems never read as one glyph. It is built once
// from letterGlyphs so the shapes stay in one place.
var wordGlyph = buildWordGlyph()

func buildWordGlyph() [bannerRows]string {
	var rows [bannerRows]string
	for index, letter := range Wordmark {
		glyph, ok := letterGlyphs[letter]
		if !ok {
			continue
		}
		for row := range rows {
			if index > 0 {
				rows[row] += "  "
			}
			rows[row] += glyph[row]
		}
	}
	return rows
}

// Banner returns the multi-line "simple" wordmark, newline-terminated and with
// no trailing spaces on any line. In ModePlain it is ASCII only: the assertion
// that it holds no escape byte is TestBannerPlainHasNoEscape.
func Banner(mode Mode) string {
	ink := "#"
	if mode != ModePlain {
		// A solid block reads as a wordmark rather than as punctuation, and a
		// terminal that reports colour renders U+2588 too.
		ink = "█"
	}
	lines := make([]string, 0, bannerRows+1)
	for row := range bannerRows {
		mark := Paint(mode, Primary, strings.ReplaceAll(markGlyph[row], "#", ink))
		word := Paint(mode, Foreground, strings.TrimRight(strings.ReplaceAll(wordGlyph[row], "#", ink), " "))
		lines = append(lines, strings.TrimRight(mark+"  "+word, " "))
	}
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// Mark returns the compact one-line mark every other command prints. It is a
// single line with no trailing newline, so a caller can put it in a header.
func Mark(mode Mode) string {
	if mode == ModePlain {
		return Wordmark
	}
	return Paint(mode, Primary, "■") + " " + Paint(mode, Foreground, Wordmark)
}

// WriteBanner prints the banner to w in the variant w can take.
func WriteBanner(w io.Writer, getenv func(string) string) error {
	_, err := io.WriteString(w, Banner(Detect(w, getenv)))
	return err
}

// WriteMark prints the one-line mark to w, newline-terminated, in the variant
// w can take.
func WriteMark(w io.Writer, getenv func(string) string) error {
	_, err := io.WriteString(w, Mark(Detect(w, getenv))+"\n")
	return err
}
