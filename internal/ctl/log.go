package ctl

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
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
// container log, even when a secret is split across Write calls.
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
		// Both terminators count: osm2pgsql reports progress with bare '\r',
		// so splitting on '\n' alone withheld it until the run finished.
		i := bytes.IndexAny(r.buf, "\r\n")
		if i < 0 {
			break
		}
		end := i + 1
		if r.buf[i] == '\r' && end < len(r.buf) && r.buf[end] == '\n' {
			end++ // CRLF is one terminator, not two
		}
		line := string(r.buf[:end])
		r.buf = r.buf[end:]
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

// RegisterURLSecrets masks passwords embedded in the configured URLs
// (https://user:pass@mirror/...), which are logged on every download.
func RegisterURLSecrets(c *Config) {
	for _, raw := range []string{c.PBFURL, c.ReplicationURL, c.MirrorBaseURL} {
		if u, err := url.Parse(raw); err == nil && u.User != nil {
			if p, ok := u.User.Password(); ok {
				RegisterSecret(p)
			}
		}
		// Logged as written, so the percent-encoded form must be masked too.
		RegisterSecret(rawURLPassword(raw))
	}
}

// rawURLPassword returns the password of raw's userinfo exactly as written.
func rawURLPassword(raw string) string {
	_, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return ""
	}
	_, pw, _ := strings.Cut(rest[:at], ":")
	return pw
}
