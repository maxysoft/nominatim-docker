package ctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
	"time"
)

// shutdownGrace is how long a child may take to exit after SIGTERM before it
// is killed; it comfortably exceeds Gunicorn's --graceful-timeout.
const shutdownGrace = 35 * time.Second

// nominatimUser is the unprivileged account created in the Dockerfile.
const nominatimUser = "nominatim"

// Runner launches child processes as an unprivileged user via a direct
// fork+setuid, so the image needs no setuid binary and can run under
// no-new-privileges.
type Runner struct {
	UID, GID int
	Dir      string
	Env      []string
}

// Command builds a child process that will run as r.UID/r.GID.
func (r *Runner) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Dir
	cmd.Env = r.Env
	// Child output goes through the secret masker too; Run flushes after exit.
	cmd.Stdout = &RedactWriter{W: os.Stdout}
	cmd.Stderr = &RedactWriter{W: os.Stderr}
	// SIGTERM first so Gunicorn drains and an import is not cut mid-transaction;
	// WaitDelay escalates to SIGKILL.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = shutdownGrace
	// Only meaningful, and only permitted, when running as root.
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
	cmd.Stdout.(*RedactWriter).Flush()
	cmd.Stderr.(*RedactWriter).Flush()
	if err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}

// WithEnv returns a copy of the runner with extra environment entries
// appended; later entries win.
func (r *Runner) WithEnv(extra ...string) *Runner {
	c := *r
	c.Env = append(append([]string{}, r.Env...), extra...)
	return &c
}

// HaveImportTools reports whether osm2pgsql is installed. The slim serve image
// ships without it, and with it goes the ability to import or replicate.
func HaveImportTools() bool {
	_, err := exec.LookPath("osm2pgsql")
	return err == nil
}

// WarnIfNoInit notes that orphan reaping is delegated to the container
// runtime: a Wait4(-1) reaper would race exec.Cmd.Wait for our own children
// and turn successful imports into intermittent ECHILD failures.
func WarnIfNoInit() {
	if os.Getpid() == 1 {
		Logf("note: running as PID 1 without an init. Orphaned processes will not be reaped; " +
			"use `docker run --init` or compose `init: true`.")
	}
}

// LookupNominatimUser resolves the unprivileged account the workload runs as.
// The UID is fixed at image build time so data volumes survive rebuilds.
func LookupNominatimUser() (uid, gid int, err error) {
	if os.Geteuid() != 0 {
		// Already unprivileged: run everything as ourselves.
		return os.Geteuid(), os.Getegid(), nil
	}

	u, err := user.Lookup(nominatimUser)
	if err != nil {
		return 0, 0, fmt.Errorf("user %q does not exist in the image: %w", nominatimUser, err)
	}
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, err
	}
	if gid, err = strconv.Atoi(u.Gid); err != nil {
		return 0, 0, err
	}
	if uid == 0 {
		return 0, 0, fmt.Errorf("user %q must not be root", nominatimUser)
	}
	return uid, gid, nil
}
