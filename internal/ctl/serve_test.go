package ctl

import (
	"strings"
	"testing"
)

// Gunicorn connects as the web role on purpose; inheriting NOMINATIM_PASSWORD
// from the container environment would hand it the CREATEDB role regardless.
func TestBaseEnvWithholdsRolePasswords(t *testing.T) {
	withEnv(t, baseEnv())
	t.Setenv("NOMINATIM_WEBUSER_PASSWORD", "web-pw")
	t.Setenv("NOMINATIM_DATABASE_DSN", "pgsql:from-operator")
	t.Setenv("NOMINATIM_QUERY_TIMEOUT", "5")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	env := strings.Join(BaseEnv(c), "\n")
	for _, k := range []string{"NOMINATIM_PASSWORD=", "NOMINATIM_WEBUSER_PASSWORD=", "NOMINATIM_DATABASE_DSN=pgsql:from-operator"} {
		if strings.Contains(env, k) {
			t.Errorf("child environment carries %s:\n%s", k, env)
		}
	}
	if !strings.Contains(env, "NOMINATIM_QUERY_TIMEOUT=5") || !strings.Contains(env, "NOMINATIM_DATABASE_DSN=pgsql:host=db;") {
		t.Errorf("expected passthrough and the generated DSN:\n%s", env)
	}
}
