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
