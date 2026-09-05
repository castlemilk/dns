package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/castlemilk/dns/internal/brand"
)

// printer decides how one command's result reaches the operator: a JSON
// document on stdout for a machine, or a branded table for a human. Nothing
// else in the CLI writes to stdout.
type printer struct {
	out    io.Writer
	err    io.Writer
	asJSON bool
	getenv func(string) string
}

func newPrinter(e *env, o *options) *printer {
	return &printer{out: e.stdout, err: e.stderr, asJSON: o.json, getenv: e.getenv}
}

// emit writes doc as JSON, or calls text to draw the human rendering under the
// one-line brand mark.
//
// The mark goes to stdout only in text mode: --json output is a document and
// nothing else, so it can be piped straight into jq.
func (p *printer) emit(doc any, text func(io.Writer) error) error {
	if p.asJSON {
		return writeJSONTo(p.out, doc)
	}
	if text == nil {
		return nil
	}
	if err := brand.WriteMark(p.out, p.getenv); err != nil {
		return err
	}
	return text(p.out)
}

// note writes a sentence for the human on stderr. It is used for the copy the
// control plane returns alongside a result ("the running site keeps the values
// it started with until the next deploy"), which belongs beside the data
// rather than inside it.
func (p *printer) note(format string, args ...any) error {
	if p.asJSON {
		return nil
	}
	_, err := fmt.Fprintf(p.err, format+"\n", args...)
	return err
}

// writeJSONTo encodes doc deterministically.
//
// protojson deliberately varies its whitespace between runs, so a proto message
// is compacted and re-indented here: the CLI's JSON contract is a stable
// document, not a stable byte string.
func writeJSONTo(w io.Writer, doc any) error {
	raw, err := encodeJSON(doc)
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n")
	return err
}

func encodeJSON(doc any) ([]byte, error) {
	if message, ok := doc.(proto.Message); ok {
		raw, err := rawProto(message)
		if err != nil {
			return nil, err
		}
		var indented bytes.Buffer
		if err := json.Indent(&indented, raw, "", "  "); err != nil {
			return nil, err
		}
		return indented.Bytes(), nil
	}
	return json.MarshalIndent(doc, "", "  ")
}

// rawProto renders a proto message with its proto field names and every field
// present, so a consumer never has to guess whether a missing key means false
// or means "the server is older than this field".
func rawProto(message proto.Message) (json.RawMessage, error) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode response: %w", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, fmt.Errorf("encode response: %w", err)
	}
	return compact.Bytes(), nil
}

// table is the human rendering: aligned columns, no colour inside the data so
// that a copied line is exactly what the control plane said.
type table struct {
	writer *tabwriter.Writer

	// err holds the first write failure. Rows are buffered by the tabwriter
	// anyway, so reporting at flush is both simpler for the caller and the
	// only point at which a failure is real.
	err error
}

func newTable(w io.Writer, headers ...string) *table {
	t := &table{writer: tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)}
	if len(headers) > 0 {
		t.row(headers...)
	}
	return t
}

func (t *table) row(cells ...string) {
	if t.err != nil {
		return
	}
	_, t.err = io.WriteString(t.writer, strings.Join(cells, "\t")+"\n")
}

func (t *table) flush() error {
	if t.err != nil {
		return t.err
	}
	return t.writer.Flush()
}

// fields draws a "name: value" block, for the show commands where a table of
// one row would be harder to read than a list.
func fields(w io.Writer, pairs ...[2]string) error {
	t := newTable(w)
	for _, pair := range pairs {
		t.row(pair[0]+":", pair[1])
	}
	return t.flush()
}

func pair(name, value string) [2]string { return [2]string{name, value} }

func writeBanner(e *env) error { return brand.WriteBanner(e.stdout, e.getenv) }

// dash is what an empty cell shows, so a column never looks like it was lost.
const dash = "-"

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return dash
	}
	return value
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func formatTime(stamp *timestamppb.Timestamp) string {
	if stamp == nil || !stamp.IsValid() || stamp.AsTime().IsZero() {
		return dash
	}
	return stamp.AsTime().UTC().Format(time.RFC3339)
}

func formatUint(value uint32) string { return strconv.FormatUint(uint64(value), 10) }

func formatBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return strconv.FormatUint(value, 10) + " B"
	}
	divisor, exponent := uint64(unit), 0
	for size := value / unit; size >= unit; size /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(divisor), "KMGTPE"[exponent])
}

// label turns a proto enum name into the word an operator types and reads:
// SITE_STATE_READY becomes "ready".
func label(enumName, prefix string) string {
	trimmed := strings.TrimPrefix(enumName, prefix)
	if trimmed == "UNSPECIFIED" || trimmed == "" {
		return dash
	}
	return strings.ToLower(trimmed)
}
