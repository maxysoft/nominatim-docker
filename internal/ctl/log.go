package ctl

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
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
	W   io.Writer
	buf []byte
}

func (r *RedactWriter) Write(p []byte) (int, error) {
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
	if len(r.buf) > 0 {
		io.WriteString(r.W, Redact(string(r.buf)))
		r.buf = r.buf[:0]
	}
}

// RegisterURLSecrets masks credentials embedded in the configured URLs, which
// are logged on every download: a userinfo password (https://user:pass@...),
// a bare userinfo token (https://TOKEN@...), and credential query values of
// presigned or SAS URLs (X-Amz-Signature=, sig=, token=).
func RegisterURLSecrets(c *Config) {
	for _, raw := range []string{c.PBFURL, c.ReplicationURL, c.MirrorBaseURL} {
		if u, err := url.Parse(raw); err == nil {
			if u.User != nil {
				if p, ok := u.User.Password(); ok {
					RegisterSecret(p)
				} else {
					RegisterSecret(u.User.Username())
				}
			}
			for _, kv := range strings.Split(u.RawQuery, "&") {
				k, v, _ := strings.Cut(kv, "=")
				// Short or flag-like values (host, true) would mask common words
				// in every log line; real credentials are longer.
				if !credentialParam(k) || len(v) < 8 {
					continue
				}
				// Both forms, as for the userinfo below.
				RegisterSecret(v)
				if dec, err := url.QueryUnescape(v); err == nil {
					RegisterSecret(dec)
				}
			}
		}
		// Logged as written, so the percent-encoded form must be masked too.
		RegisterSecret(rawURLPassword(raw))
	}
}

// credentialParam reports whether a query key names a credential. Exact names:
// a substring test would also catch X-Amz-SignedHeaders or passive.
func credentialParam(key string) bool {
	return slices.Contains([]string{"sig", "signature", "x-amz-signature", "x-amz-credential", "x-amz-security-token",
		"x-goog-signature", "x-goog-credential", "token", "access_token", "api_key", "apikey", "key", "secret",
		"password", "auth"}, strings.ToLower(key))
}

// rawURLPassword returns the secret half of raw's userinfo exactly as written:
// the password, or the whole userinfo when it is a bare token.
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
	user, pw, ok := strings.Cut(rest[:at], ":")
	if !ok {
		return user
	}
	return pw
}
