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

var debug bool

// SetDebug enables verbose logging. DEBUG_MODE used to turn on `set -x`, which
// echoed every password into the container logs; this prints the same kind of
// detail with secrets masked.
func SetDebug(on bool) { debug = on }

// Debugf writes a redacted line only when DEBUG_MODE is enabled.
func Debugf(format string, args ...any) {
	if debug {
		fmt.Fprintln(os.Stdout, "debug: "+Redact(fmt.Sprintf(format, args...)))
	}
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
	W   io.Writer
	buf []byte
}

func (r *RedactWriter) Write(p []byte) (int, error) {
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
	if len(r.buf) > 0 {
		io.WriteString(r.W, Redact(string(r.buf)))
		r.buf = r.buf[:0]
	}
}
