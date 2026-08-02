package ctl

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

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
		envOr("NOMINATIM_USER", "nominatim"), uid, bad, uid, gid)
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
