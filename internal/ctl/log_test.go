package ctl

import (
	"strings"
	"testing"
)

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

func TestRedactIgnoresTinyValues(t *testing.T) {
	RegisterSecret("ab")
	if got := Redact("a table of abbreviations"); got != "a table of abbreviations" {
		t.Fatalf("short secret altered output: %s", got)
	}
}

func TestRedactWriterMasksSplitChildOutput(t *testing.T) {
	RegisterSecret("split-secret-here")
	var sink strings.Builder
	w := &RedactWriter{W: &sink}

	w.Write([]byte("prefix split-"))
	w.Write([]byte("secret-here suffix\n"))
	if strings.Contains(sink.String(), "split-secret-here") {
		t.Fatalf("secret survived a split write: %s", sink.String())
	}
	if !strings.Contains(sink.String(), "***") {
		t.Fatalf("no redaction marker: %s", sink.String())
	}
}

func TestRedactWriterSplitsOnCarriageReturn(t *testing.T) {
	var sink strings.Builder
	w := &RedactWriter{W: &sink}

	w.Write([]byte("Processing: Node(1k)\rProcessing: Node(2k)\r"))
	if got := sink.String(); got != "Processing: Node(1k)\rProcessing: Node(2k)\r" {
		t.Fatalf("progress not emitted per \\r: %q", got)
	}

	sink.Reset()
	w.Write([]byte("done\r\nnext\n"))
	if got := sink.String(); got != "done\r\nnext\n" {
		t.Fatalf("CRLF mishandled: %q", got)
	}
}

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

func TestRegisterURLSecrets(t *testing.T) {
	RegisterURLSecrets(&Config{PBFURL: "https://user:mirror-secret-42@mirror.example/planet.pbf"})
	if got := Redact("downloading https://user:mirror-secret-42@mirror.example/planet.pbf"); strings.Contains(got, "mirror-secret-42") {
		t.Fatalf("password not masked: %s", got)
	}
	RegisterURLSecrets(&Config{ReplicationURL: "https://u:p%40ss-secret-77@mirror.example/updates/"})
	for _, form := range []string{"p%40ss-secret-77", "p@ss-secret-77"} {
		if got := Redact("x " + form + " y"); strings.Contains(got, form) {
			t.Fatalf("%s not masked: %s", form, got)
		}
	}
}

func TestRegisterURLSecretsTokensAndQuery(t *testing.T) {
	RegisterURLSecrets(&Config{
		PBFURL:        "https://bucket.example/p.pbf?X-Amz-Credential=AKIDEXAMPLE%2F20260101&X-Amz-Signature=deadbeef1234&format=jsonish",
		MirrorBaseURL: "https://ghp-token-5678@mirror.example/data",
	})
	for _, form := range []string{"AKIDEXAMPLE%2F20260101", "AKIDEXAMPLE/20260101", "deadbeef1234", "ghp-token-5678"} {
		if got := Redact("x " + form + " y"); strings.Contains(got, form) {
			t.Errorf("%s not masked: %s", form, got)
		}
	}
	if got := Redact("format=jsonish"); got != "format=jsonish" {
		t.Errorf("non-credential query value masked: %s", got)
	}
	RegisterURLSecrets(&Config{PBFURL: "https://b.example/p.pbf?X-Amz-SignedHeaders=host&auth=true"})
	if got := Redact("lookup host: true"); got != "lookup host: true" {
		t.Errorf("common words masked: %s", got)
	}
}
