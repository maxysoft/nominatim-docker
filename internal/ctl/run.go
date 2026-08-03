package ctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// shutdownGrace is how long a child may take to exit after SIGTERM before it
// is killed. It comfortably exceeds Gunicorn's own --graceful-timeout.
const shutdownGrace = 35 * time.Second

// nominatimUser is the unprivileged account, created in the Dockerfile.
const nominatimUser = "nominatim"

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
}

// Command builds a child process that will run as r.UID/r.GID.
func (r *Runner) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Dir
	cmd.Env = r.Env
	// Child output is filtered too: a Nominatim traceback or an osm2pgsql error
	// can otherwise echo a DSN straight into the container log. One pair per
	// child; Run flushes them after the child exits.
	cmd.Stdout = &RedactWriter{W: os.Stdout}
	cmd.Stderr = &RedactWriter{W: os.Stderr}
	// Cancelling the context asks the child to stop rather than killing it, so
	// Gunicorn drains and a long import is not truncated mid-transaction.
	// WaitDelay escalates to SIGKILL if it ignores that.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = shutdownGrace
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
	cmd.Stdout.(*RedactWriter).Flush()
	cmd.Stderr.(*RedactWriter).Flush()
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

// HaveImportTools reports whether osm2pgsql is installed. The slim serve image
// (Dockerfile --target serve) ships without it, and with it goes the ability
// to run an import or apply replication diffs.
func HaveImportTools() bool {
	_, err := exec.LookPath("osm2pgsql")
	return err == nil
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

// LookupNominatimUser resolves the unprivileged account the workload runs as.
//
// The account is created at image build time with a fixed UID so that data
// volumes keep working across rebuilds. The shell version created it at runtime
// with whatever UID the kernel happened to assign.
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

// writableBy reports whether uid/gid may write to the file at path.
func writableBy(fi os.FileInfo, uid, gid int) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	mode := fi.Mode().Perm()
	switch {
	case int(st.Uid) == uid:
		return mode&0o200 != 0
	case int(st.Gid) == gid:
		return mode&0o020 != 0
	default:
		return mode&0o002 != 0
	}
}

// EnsureVolumeOwnership checks that the unprivileged user can write everything
// it needs to, and either repairs it or explains how.
//
// A volume written by the pre-refactor image contains root-owned files, because
// that image ran as root throughout. chownProjectFiles is deliberately
// non-recursive so it cannot rewrite ownership of operator data bind-mounted
// under the project directory — which means it also cannot repair those files.
// Without this check the failure surfaces much later as an opaque permission
// error from deep inside Nominatim.
func EnsureVolumeOwnership(c *Config, uid, gid int) error {
	if os.Geteuid() != 0 {
		return nil // not privileged enough to fix anything; let it fail naturally
	}

	paths := []string{c.ProjectDir}
	if c.FlatnodeFile != "" {
		paths = append(paths, filepath.Dir(c.FlatnodeFile))
	}

	if c.FixVolumeOwnership {
		for _, p := range paths {
			Logf("FIX_VOLUME_OWNERSHIP=true — recursively taking ownership of %s", p)
			if err := chownTree(p, uid, gid); err != nil {
				return err
			}
		}
		return nil
	}

	var bad []string
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !writableBy(fi, uid, gid) {
			bad = append(bad, p)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("the %q user (uid %d) cannot write to %v.\n"+
		"A volume created by an older release of this image is owned by root. Fix it once with:\n"+
		"    docker run --rm -v <your-volume>:/nominatim alpine chown -R %d:%d /nominatim\n"+
		"or start the container with FIX_VOLUME_OWNERSHIP=true to have it done for you.\n"+
		"Note that the latter also rewrites ownership of anything you have bind-mounted underneath",
		nominatimUser, uid, bad, uid, gid)
}

// chownTree recursively changes ownership without following symlinks.
func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped rather than fatal
		}
		if lchownErr := os.Lchown(p, uid, gid); lchownErr != nil {
			return fmt.Errorf("chown %s: %w", p, lchownErr)
		}
		return nil
	})
}
