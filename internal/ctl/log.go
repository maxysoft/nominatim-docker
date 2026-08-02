package ctl

import (
	"fmt"
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
