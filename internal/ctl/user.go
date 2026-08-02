package ctl

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
)

// LookupNominatimUser resolves the unprivileged account the workload runs as.
//
// The account is created at image build time with a fixed UID so that data
// volumes keep working across rebuilds. The shell version created it at runtime
// with whatever UID the kernel happened to assign.
func LookupNominatimUser() (uid, gid int, err error) {
	name := envOr("NOMINATIM_USER", "nominatim")

	if os.Geteuid() != 0 {
		// Already unprivileged: run everything as ourselves.
		return os.Geteuid(), os.Getegid(), nil
	}

	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("user %q does not exist in the image: %w", name, err)
	}
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, err
	}
	if gid, err = strconv.Atoi(u.Gid); err != nil {
		return 0, 0, err
	}
	if uid == 0 {
		return 0, 0, fmt.Errorf("user %q must not be root", name)
	}
	return uid, gid, nil
}
