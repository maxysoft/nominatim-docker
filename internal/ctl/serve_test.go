package ctl

import (
	"net/http"
	"net/http/httptest"
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
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
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
	if !strings.Contains(env, "NOMINATIM_QUERY_TIMEOUT=5") || !strings.Contains(env, "HTTPS_PROXY=http://proxy:3128") || !strings.Contains(env, "NOMINATIM_DATABASE_DSN=pgsql:host=db;") {
		t.Errorf("expected passthrough and the generated DSN:\n%s", env)
	}
}

// status.php answers 200 even when the database is unreachable; the
// healthcheck must read the body, or a broken API stays "healthy".
func TestHealthcheckReadsStatusBody(t *testing.T) {
	for body, wantOK := range map[string]bool{
		`{"status":0,"message":"OK","data_updated":"2026-01-01T00:00:00+00:00"}`: true,
		`{"status":700,"message":"Database connection failed"}`:                  false,
		`<html>oops</html>`: false,
		`{}`:                false,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		}))
		err := Healthcheck(strings.TrimPrefix(srv.URL, "http://"))
		srv.Close()
		if (err == nil) != wantOK {
			t.Errorf("body %s: err = %v, want ok=%v", body, err, wantOK)
		}
	}
}
