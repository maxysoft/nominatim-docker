package ctl

import (
	"strings"
	"testing"
)

// DEBUG_MODE used to enable `set -x`, echoing every password into the container
// logs. Anything this process prints is filtered instead.
func TestRedact(t *testing.T) {
	RegisterSecret("hunter2-long-enough")
	out := Redact(`connecting with password=hunter2-long-enough to db`)
	if strings.Contains(out, "hunter2-long-enough") {
		t.Fatalf("secret survived redaction: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Fatalf("no redaction marker: %s", out)
	}
}

// Very short values would match too much unrelated text.
func TestRedactIgnoresTinyValues(t *testing.T) {
	RegisterSecret("ab")
	if got := Redact("a table of abbreviations"); got != "a table of abbreviations" {
		t.Fatalf("short secret altered output: %s", got)
	}
}

// A Nominatim traceback or an osm2pgsql error can echo a DSN; child output is
// filtered too, not just what the entrypoint logs itself.
func TestRedactWriterMasksChildOutput(t *testing.T) {
	RegisterSecret("child-secret-value")
	var sink strings.Builder
	w := &RedactWriter{W: &sink}

	w.Write([]byte("connecting password=child-secret-value to db\n"))
	if strings.Contains(sink.String(), "child-secret-value") {
		t.Fatalf("secret survived: %s", sink.String())
	}
	if !strings.Contains(sink.String(), "***") {
		t.Fatalf("no redaction marker: %s", sink.String())
	}
}

// A secret split across two Write calls must still be caught, which is why the
// writer buffers to line boundaries.
func TestRedactWriterHandlesSplitWrites(t *testing.T) {
	RegisterSecret("split-secret-here")
	var sink strings.Builder
	w := &RedactWriter{W: &sink}

	w.Write([]byte("prefix split-"))
	w.Write([]byte("secret-here suffix\n"))
	if strings.Contains(sink.String(), "split-secret-here") {
		t.Fatalf("secret survived a split write: %s", sink.String())
	}
}

// osm2pgsql reports progress with bare '\r'. Splitting on '\n' alone buffered
// the whole progress stream until the run ended, so a 21-minute import showed
// nothing and then dumped one enormous line.
func TestRedactWriterSplitsOnCarriageReturn(t *testing.T) {
	var sink strings.Builder
	w := &RedactWriter{W: &sink}

	w.Write([]byte("Processing: Node(1k)\rProcessing: Node(2k)\r"))
	if got := sink.String(); got != "Processing: Node(1k)\rProcessing: Node(2k)\r" {
		t.Fatalf("progress not emitted per \\r: %q", got)
	}

	// CRLF must not produce a spurious empty line.
	sink.Reset()
	w.Write([]byte("done\r\nnext\n"))
	if got := sink.String(); got != "done\r\nnext\n" {
		t.Fatalf("CRLF mishandled: %q", got)
	}
}

// A child that exits without a trailing newline must not lose its last line.
func TestRedactWriterFlushesPartialLine(t *testing.T) {
	var sink strings.Builder
	w := &RedactWriter{W: &sink}
	w.Write([]byte("no trailing newline"))
	if sink.String() != "" {
		t.Fatal("partial line was written before Flush")
	}
	w.Flush()
	if sink.String() != "no trailing newline" {
		t.Fatalf("Flush produced %q", sink.String())
	}
}
