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

const nominatimUser = "nominatim"

type Runner struct {
	UID, GID int
	Dir      string
	Env      []string
	Grace    time.Duration
}

func (r *Runner) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Dir
	cmd.Env = r.Env
	cmd.Stdout = &RedactWriter{W: os.Stdout}
	cmd.Stderr = &RedactWriter{W: os.Stderr}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = r.Grace
	if os.Geteuid() == 0 && r.UID != 0 {
		// Keep the empty Groups below: it makes Go call setgroups and drop root's supplementary groups (gid 0, group_add).
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid:    uint32(r.UID),
				Gid:    uint32(r.GID),
				Groups: []uint32{},
			},
		}
	}
	return cmd
}

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

func (r *Runner) WithEnv(extra ...string) *Runner {
	c := *r
	c.Env = append(append([]string{}, r.Env...), extra...)
	return &c
}

func HaveImportTools() bool {
	_, err := exec.LookPath("osm2pgsql")
	return err == nil
}

// Never add a Wait4(-1) reaper: it races exec.Cmd.Wait and fails imports with ECHILD.
func WarnIfNoInit() {
	if os.Getpid() == 1 {
		Logf("note: running as PID 1 without an init. Orphaned processes will not be reaped; " +
			"use `docker run --init` or compose `init: true`.")
	}
}

func LookupNominatimUser() (uid, gid int, err error) {
	if os.Geteuid() != 0 {
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
