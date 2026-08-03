// Command nominatim-ctl is the container entrypoint for nominatim-docker: it
// renders the project configuration, provisions the external PostgreSQL
// database, runs the import when one is needed, and supervises the API server.
package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/maxysoft/nominatim-docker/internal/ctl"
)

func main() {
	if err := run(); err != nil {
		ctl.Errf("fatal: %v", err)
		os.Exit(1)
	}
}

func run() error {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	if cmd == "healthcheck" {
		bind := "127.0.0.1:8080"
		if len(os.Args) > 2 {
			bind = os.Args[2]
		} else if b := os.Getenv("GUNICORN_BIND"); b != "" {
			bind = loopback(b)
		}
		return ctl.Healthcheck(bind)
	}

	c, err := ctl.Load()
	if err != nil {
		return err
	}
	ctl.RegisterSecret(c.NominatimPassword)
	ctl.RegisterSecret(c.AdminPassword)
	ctl.RegisterSecret(c.WebUserPassword)

	// Installed before any long-running work, so a stop during a multi-day
	// import still exits cleanly instead of dying with exit 2.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	ctl.WarnIfNoInit()

	var runErr error
	switch cmd {
	case "", "serve":
		runErr = ctl.Serve(ctx, c)
	case "import":
		uid, gid, lookupErr := ctl.LookupNominatimUser()
		if lookupErr != nil {
			return lookupErr
		}
		r := &ctl.Runner{UID: uid, GID: gid, Dir: c.ProjectDir, Env: ctl.BaseEnv(c)}
		if runErr = ctl.PrepareProjectDir(c, uid, gid); runErr == nil {
			runErr = ctl.RunImport(ctx, c, r)
		}
	case "config":
		_, err := os.Stdout.WriteString(ctl.RenderEnvFile(c))
		return err
	default:
		ctl.Errf("usage: nominatim-ctl [serve|import|config|healthcheck [host:port]]")
		os.Exit(2)
	}

	// A stop requested by the orchestrator is a successful exit, whatever the
	// interrupted child reported.
	if ctx.Err() != nil {
		ctl.Logf("shutdown complete")
		return nil
	}
	return runErr
}

// loopback rewrites the bind address into one reachable from inside the
// container, so the healthcheck follows GUNICORN_BIND.
func loopback(bind string) string {
	if strings.HasPrefix(bind, ":") {
		return "127.0.0.1" + bind
	}
	if strings.HasPrefix(bind, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(bind, "0.0.0.0:")
	}
	return bind
}
