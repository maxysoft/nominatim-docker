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
// DEBUG_MODE previously enabled `set -x`, which echoed every password to stdout
// and therefore into the container logs.
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

// RedactWriter filters a child process's output through the secret masker.
//
// Only what this process logs itself was previously redacted; a Python
// traceback from Nominatim, or an osm2pgsql error echoing a connection string,
// went to the container log verbatim. Filtering is line-oriented, so a secret
// split across two Write calls is still caught.
type RedactWriter struct {
	W io.Writer

	// Each child gets its own writer pair, so today nothing contends. The lock
	// stays because it costs nothing uncontended and makes the type safe if a
	// writer is ever shared — an earlier draft did exactly that and raced.
	mu  sync.Mutex
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
	// Cap the pending partial line so a child emitting a very long line without
	// a newline cannot grow this buffer without bound.
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
