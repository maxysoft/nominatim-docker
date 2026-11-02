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

func RegisterSecret(s string) {
	if len(s) < 4 {
		return
	}
	redactMu.Lock()
	defer redactMu.Unlock()
	secrets = append(secrets, s)
}

func Redact(s string) string {
	redactMu.RLock()
	defer redactMu.RUnlock()
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, "***")
	}
	return s
}

func Logf(format string, args ...any) {
	fmt.Fprintln(os.Stdout, Redact(fmt.Sprintf(format, args...)))
}

func Errf(format string, args ...any) {
	fmt.Fprintln(os.Stderr, Redact(fmt.Sprintf(format, args...)))
}

type RedactWriter struct {
	W   io.Writer
	buf []byte
}

func (r *RedactWriter) Write(p []byte) (int, error) {
	r.buf = append(r.buf, p...)
	for {
		i := bytes.IndexAny(r.buf, "\r\n")
		if i < 0 {
			break
		}
		end := i + 1
		if r.buf[i] == '\r' && end < len(r.buf) && r.buf[end] == '\n' {
			end++
		}
		line := string(r.buf[:end])
		r.buf = r.buf[end:]
		if _, err := io.WriteString(r.W, Redact(line)); err != nil {
			return 0, err
		}
	}
	if len(r.buf) > 1<<20 {
		if _, err := io.WriteString(r.W, Redact(string(r.buf))); err != nil {
			return 0, err
		}
		r.buf = r.buf[:0]
	}
	return len(p), nil
}

func (r *RedactWriter) Flush() {
	if len(r.buf) > 0 {
		io.WriteString(r.W, Redact(string(r.buf)))
		r.buf = r.buf[:0]
	}
}

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
				if !credentialParam(k) || len(v) < 8 {
					continue
				}
				RegisterSecret(v)
				if dec, err := url.QueryUnescape(v); err == nil {
					RegisterSecret(dec)
				}
			}
		}
		RegisterSecret(rawURLPassword(raw))
	}
}

func credentialParam(key string) bool {
	return slices.Contains([]string{"sig", "signature", "x-amz-signature", "x-amz-credential", "x-amz-security-token",
		"x-goog-signature", "x-goog-credential", "token", "access_token", "api_key", "apikey", "key", "secret",
		"password", "auth"}, strings.ToLower(key))
}

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
