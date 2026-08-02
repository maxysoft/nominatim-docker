package ctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Runner launches child processes as an unprivileged user.
//
// The shell implementation used `sudo -E -u nominatim` for this. sudo is a
// setuid-root binary reachable by the very user Gunicorn runs as, and it exists
// in the image solely to move privilege downwards — something a direct
// fork+setuid does without any setuid binary at all. Dropping it also lets the
// container run under no-new-privileges.
type Runner struct {
	UID, GID int
	Dir      string
	Env      []string

	// pending holds the output filters of commands this runner started, so a
	// trailing partial line is not lost when the child exits.
	pending []*RedactWriter
}

// FlushOutput writes any buffered partial lines from finished children.
func (r *Runner) FlushOutput() {
	for _, w := range r.pending {
		w.Flush()
	}
	r.pending = nil
}

// Command builds a child process that will run as r.UID/r.GID.
func (r *Runner) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Dir
	cmd.Env = r.Env
	// Child output is filtered too: a Nominatim traceback or an osm2pgsql error
	// can otherwise echo a DSN straight into the container log.
	outw, errw := &RedactWriter{W: os.Stdout}, &RedactWriter{W: os.Stderr}
	cmd.Stdout, cmd.Stderr = outw, errw
	r.pending = append(r.pending, outw, errw)
	// Cancelling the context asks the child to stop rather than killing it, so
	// Gunicorn drains and a long import is not truncated mid-transaction.
	// WaitDelay escalates to SIGKILL if it ignores that.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = gracePeriod()
	// Credential is only meaningful, and only permitted, when we start as root.
	if os.Geteuid() == 0 && r.UID != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid:         uint32(r.UID),
				Gid:         uint32(r.GID),
				NoSetGroups: true,
			},
		}
	}
	return cmd
}

// Run executes a command to completion, returning a wrapped error on failure.
func (r *Runner) Run(ctx context.Context, name string, args ...string) error {
	Logf("+ %s %v", name, args)
	cmd := r.Command(ctx, name, args...)
	err := cmd.Run()
	r.FlushOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}

// WithEnv returns a copy of the runner with extra environment entries appended.
// Later entries win, which is how the API process gets a different DSN from the
// administrative CLI invocations.
func (r *Runner) WithEnv(extra ...string) *Runner {
	c := *r
	c.Env = append(append([]string{}, r.Env...), extra...)
	return &c
}

// gracePeriod is how long a child may take to exit after being asked to.
func gracePeriod() time.Duration {
	if v, err := time.ParseDuration(envOr("SHUTDOWN_GRACE", "35s")); err == nil {
		return v
	}
	return 35 * time.Second
}

// WarnIfNoInit notes that orphan reaping is delegated to the container runtime.
//
// A generic `Wait4(-1, WNOHANG)` reaper cannot be combined with os/exec: it
// races exec.Cmd.Wait for the exit status of our own children, and whichever
// call loses gets ECHILD. That would intermittently report a successful import
// as a failure, which is far worse than the zombies it would prevent. Docker's
// `--init` (compose `init: true`) puts a real init at PID 1 and solves the
// problem outside this process.
func WarnIfNoInit() {
	if os.Getpid() == 1 {
		Logf("note: running as PID 1 without an init. Orphaned processes will not be reaped; " +
			"use `docker run --init` or compose `init: true`.")
	}
}
