package main

import (
	"context"
	"fmt"
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
	ctl.RegisterURLSecrets(c)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	ctl.WarnIfNoInit()

	var runErr error
	switch cmd {
	case "", "serve":
		runErr = ctl.Serve(ctx, c)
	case "import", "reimport":
		r, err := ctl.NewRunner(c)
		if err != nil {
			return err
		}
		if cmd == "reimport" {
			c.AllowDropExistingDB = true
			runErr = ctl.RunImport(ctx, c, r)
		} else {
			runErr = ctl.EnsureImported(ctx, c, r)
		}
	case "replicate":
		runErr = ctl.Replicate(ctx, c)
	case "config":
		_, err := os.Stdout.WriteString(ctl.RenderEnvFile(c))
		return err
	default:
		ctl.Errf("usage: nominatim-ctl [serve|import|reimport|replicate|config|healthcheck [host:port]]")
		os.Exit(2)
	}

	if ctx.Err() != nil {
		if (cmd == "import" || cmd == "reimport") && runErr != nil {
			return fmt.Errorf("import interrupted before completion: %w", runErr)
		}
		ctl.Logf("shutdown complete")
		return nil
	}
	return runErr
}

func loopback(bind string) string {
	if strings.HasPrefix(bind, ":") {
		return "127.0.0.1" + bind
	}
	if strings.HasPrefix(bind, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(bind, "0.0.0.0:")
	}
	return bind
}
