package ctl

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

var (
	redactMu sync.RWMutex
	secrets  []string
)

// RegisterSecret marks a value to be masked in anything this process logs.
// Values shorter than four characters are ignored as masking noise.
func RegisterSecret(s string) {
	if len(s) < 4 {
		return
	}
	redactMu.Lock()
	defer redactMu.Unlock()
	secrets = append(secrets, s)
}

// Redact replaces every registered secret in s with a placeholder.
func Redact(s string) string {
	redactMu.RLock()
	defer redactMu.RUnlock()
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, "***")
	}
	return s
}

// Logf writes a redacted line to stdout.
func Logf(format string, args ...any) {
	fmt.Fprintln(os.Stdout, Redact(fmt.Sprintf(format, args...)))
}

// Errf writes a redacted line to stderr.
func Errf(format string, args ...any) {
	fmt.Fprintln(os.Stderr, Redact(fmt.Sprintf(format, args...)))
}

// RedactWriter filters a child process's output through the secret masker,
// line by line, so a traceback or driver error cannot echo a DSN into the
// container log — even when a secret is split across Write calls.
type RedactWriter struct {
	W io.Writer

	mu  sync.Mutex // writers are per-child today; the lock keeps sharing safe
	buf []byte
}

func (r *RedactWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	for {
		i := bytes.IndexByte(r.buf, '\n')
		if i < 0 {
			break
		}
		line := string(r.buf[:i+1])
		r.buf = r.buf[i+1:]
		if _, err := io.WriteString(r.W, Redact(line)); err != nil {
			return 0, err
		}
	}
	// Flush an over-long partial line so the buffer cannot grow unbounded.
	if len(r.buf) > 1<<20 {
		if _, err := io.WriteString(r.W, Redact(string(r.buf))); err != nil {
			return 0, err
		}
		r.buf = r.buf[:0]
	}
	return len(p), nil
}

// Flush writes any trailing partial line.
func (r *RedactWriter) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) > 0 {
		io.WriteString(r.W, Redact(string(r.buf)))
		r.buf = r.buf[:0]
	}
}
