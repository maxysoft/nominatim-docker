#!/usr/bin/env bash
#
# Local integration test: builds the image, imports Monaco into a throwaway
# PostGIS container, and asserts the same behaviour the CI matrix checks.
#
# Usage: test/integration.sh [scenario ...]
# Scenarios: full security restart volume_loss serve_image shutdown failfast
#            unicode_password

set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE="docker compose -f test/docker-compose.test.yml"
# Overridable: 8080 is commonly already taken on a dev host.
ITEST_PORT="${ITEST_PORT:-18080}"
export ITEST_PORT
BASE_URL="http://127.0.0.1:${ITEST_PORT}"
IMAGE="nominatim-itest:local"

pass=0
fail=0

log()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$*"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$*"; fail=$((fail + 1)); }

cleanup() {
  docker rm -f itest-serve >/dev/null 2>&1 || true
  docker volume rm -f nominatim-itest-serve >/dev/null 2>&1 || true
  $COMPOSE down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

# wait_for_api waits until /status.php reports ready, or the container dies.
wait_for_api() {
  local deadline=$((SECONDS + ${1:-900}))
  while ((SECONDS < deadline)); do
    if ! $COMPOSE ps --status running --services 2>/dev/null | grep -q '^nominatim$'; then
      log "nominatim container is no longer running"
      $COMPOSE logs --tail 60 nominatim
      return 1
    fi
    if curl -fsS --max-time 5 "$BASE_URL/status.php?format=json" >/dev/null 2>&1; then
      return 0
    fi
    sleep 5
  done
  log "timed out waiting for the API"
  $COMPOSE logs --tail 60 nominatim
  return 1
}

# assert_json_nonempty URL DESCRIPTION
assert_json_nonempty() {
  local body
  body=$(curl -fsS --max-time 30 "$BASE_URL$1" 2>/dev/null || true)
  if [[ -n "$body" && "$body" != "[]" && "$body" != "{}" ]]; then
    ok "$2"
  else
    bad "$2 (got: ${body:0:120})"
  fi
}

# assert_contains URL PATTERN DESCRIPTION
assert_contains() {
  local body
  body=$(curl -fsS --max-time 30 "$BASE_URL$1" 2>/dev/null || true)
  if grep -qi -- "$2" <<<"$body"; then
    ok "$3"
  else
    bad "$3 (pattern '$2' absent from: ${body:0:160})"
  fi
}

build_image() {
  log "building $IMAGE"
  DOCKER_BUILDKIT=1 docker build -t "$IMAGE" .
}

scenario_full() {
  log "scenario: full import + API surface"
  cleanup
  $COMPOSE up -d
  wait_for_api 1200 || { bad "API never became ready"; return; }

  assert_json_nonempty "/status.php?format=json"                              "status endpoint answers"
  assert_contains      "/status.php?format=json" '"status":0'                 "status reports 0"
  assert_contains      "/status.php?format=json" '"software_version":"5\.'    "software_version is 5.x"
  assert_contains      "/status.php?format=json" '"database_version":"5\.'    "database_version is 5.x"
  assert_json_nonempty "/search.php?q=avenue%20pasteur"                       "forward search finds a street"
  assert_contains      "/search.php?q=Monaco&format=json&limit=1" '"class":"boundary"' "search returns the boundary"
  assert_json_nonempty "/reverse.php?lat=43.734&lon=7.42&format=json"         "reverse geocoding answers"
  assert_contains      "/reverse.php?lat=43.7314&lon=7.4197&format=json" 'Monaco' "reverse resolves to Monaco"
  assert_json_nonempty "/lookup.php?osm_ids=R1124039&format=json"             "lookup answers"
  assert_contains      "/details.php?osmtype=R&osmid=1124039&format=json" '"category":"boundary"' "details answers"

  if curl -sI --max-time 15 "$BASE_URL/status.php?format=json" | grep -qi 'application/json'; then
    ok "Content-Type is application/json"
  else
    bad "Content-Type is not application/json"
  fi

  if $COMPOSE exec -T -u nominatim nominatim nominatim admin --check-database --project-dir /nominatim >/dev/null 2>&1; then
    ok "nominatim admin --check-database succeeds"
  else
    bad "nominatim admin --check-database failed"
  fi
}

# The application role must no longer be a superuser, and the API must not be
# connecting as it.
scenario_security() {
  log "scenario: privilege and hardening"

  local super
  super=$($COMPOSE exec -T postgres psql -U postgres -tAc \
    "SELECT rolsuper FROM pg_roles WHERE rolname='nominatim'" 2>/dev/null | tr -d '[:space:]' || true)
  if [[ "$super" == "f" ]]; then
    ok "role 'nominatim' is not a superuser"
  else
    bad "role 'nominatim' rolsuper=$super (expected f)"
  fi

  if $COMPOSE exec -T postgres psql -U postgres -tAc \
      "SELECT 1 FROM pg_roles WHERE rolname='www-data'" 2>/dev/null | grep -q 1; then
    ok "read-only web role exists"
  else
    bad "web role 'www-data' missing"
  fi

  local api_users
  api_users=$($COMPOSE exec -T postgres psql -U postgres -tAc \
    "SELECT DISTINCT usename FROM pg_stat_activity WHERE datname='nominatim' AND usename IS NOT NULL" 2>/dev/null | tr -d '[:space:]' || true)
  if grep -q 'www-data' <<<"$api_users"; then
    ok "API is connected as the read-only web role"
  else
    bad "no www-data connections observed (saw: ${api_users:-none})"
  fi

  # No setuid *or setgid* binaries: the image must be safe under
  # no-new-privileges. Checking only -4000 would miss the setgid bit the
  # Dockerfile also strips.
  local setuid
  setuid=$($COMPOSE exec -T nominatim find / -xdev \( -perm -4000 -o -perm -2000 \) -type f 2>/dev/null | head -5 || true)
  if [[ -z "$setuid" ]]; then
    ok "image contains no setuid/setgid binaries"
  else
    bad "setuid/setgid binaries present: $setuid"
  fi

  for binary in sudo sshpass scp ssh curl; do
    if $COMPOSE exec -T nominatim sh -c "command -v $binary" >/dev/null 2>&1; then
      bad "$binary is still present in the image"
    else
      ok "$binary removed from the image"
    fi
  done

  # The rendered config must be owner-readable only and free of placeholders.
  local mode
  mode=$($COMPOSE exec -T nominatim stat -c '%a' /nominatim/.env 2>/dev/null | tr -d '[:space:]' || true)
  if [[ "$mode" == "600" ]]; then
    ok ".env is mode 600"
  else
    bad ".env is mode ${mode:-missing} (expected 600)"
  fi

  if $COMPOSE exec -T nominatim grep -q '__' /nominatim/.env 2>/dev/null; then
    bad "placeholder token left in .env"
  else
    ok "no placeholder tokens in .env"
  fi

  # The workload must not be running as root.
  # Read the UID numerically from /proc inside the container. `docker top`
  # resolves UIDs against the *host* passwd file, so it reports whatever the
  # host calls uid 1000 rather than "nominatim".
  local api_uid
  # shellcheck disable=SC2016  # $d must expand in the container's shell, not here
  api_uid=$($COMPOSE exec -T nominatim sh -c \
    'for d in /proc/[0-9]*; do if grep -qa gunicorn "$d/cmdline" 2>/dev/null; then stat -c %u "$d"; break; fi; done' \
    2>/dev/null | tr -d '[:space:]' || true)
  if [[ -n "$api_uid" && "$api_uid" != "0" ]]; then
    ok "gunicorn runs unprivileged (uid $api_uid)"
  else
    bad "gunicorn uid is '${api_uid:-unknown}' (must be non-root)"
  fi
}

# Restarting must reuse the existing database rather than dropping it.
scenario_restart() {
  log "scenario: restart reuses the existing import"

  local before
  before=$($COMPOSE exec -T postgres psql -U postgres -d nominatim -tAc \
    "SELECT count(*) FROM placex" 2>/dev/null | tr -d '[:space:]' || true)

  $COMPOSE restart nominatim >/dev/null
  wait_for_api 300 || { bad "API did not come back after restart"; return; }

  local after
  after=$($COMPOSE exec -T postgres psql -U postgres -d nominatim -tAc \
    "SELECT count(*) FROM placex" 2>/dev/null | tr -d '[:space:]' || true)

  if [[ -n "$before" && "$before" == "$after" ]]; then
    ok "placex row count unchanged across restart ($before)"
  else
    bad "placex changed across restart: $before -> $after"
  fi

  # Poll: there is a short lag between the container writing a line and
  # `docker compose logs` being able to read it back.
  local found=0
  for _ in $(seq 1 30); do
    if $COMPOSE logs nominatim 2>/dev/null | grep -q 'skipping import'; then
      found=1
      break
    fi
    sleep 1
  done
  if [[ $found -eq 1 ]]; then
    ok "restart skipped the import"
  else
    bad "restart did not report skipping the import"
  fi
}

# Losing the application volume must NOT drop a populated database.
scenario_volume_loss() {
  log "scenario: losing the project volume does not drop the database"

  $COMPOSE stop nominatim >/dev/null
  $COMPOSE rm -f nominatim >/dev/null
  docker volume rm -f nominatim-itest_project >/dev/null 2>&1 || true

  $COMPOSE up -d nominatim >/dev/null
  if wait_for_api 300; then
    local rows
    rows=$($COMPOSE exec -T postgres psql -U postgres -d nominatim -tAc \
      "SELECT count(*) FROM placex" 2>/dev/null | tr -d '[:space:]' || true)
    if [[ -n "$rows" && "$rows" -gt 0 ]]; then
      ok "database survived loss of the project volume ($rows rows)"
    else
      bad "database was wiped after the project volume was removed"
    fi
  else
    bad "container did not recover after the project volume was removed"
  fi
}

# The slim --target serve image must serve an existing import, but refuse to
# create one. Runs while the stack from earlier scenarios is up and imported.
scenario_serve_image() {
  log "scenario: serve-only image"

  log "building $IMAGE-serve"
  DOCKER_BUILDKIT=1 docker build --target serve -t "$IMAGE-serve" .

  for binary in osm2pgsql psql; do
    if docker run --rm --entrypoint sh "$IMAGE-serve" -c "command -v $binary" >/dev/null 2>&1; then
      bad "$binary is still present in the serve image"
    else
      ok "$binary absent from the serve image"
    fi
  done

  # Against a database that holds no import it must fail fast and say why,
  # before downloading or provisioning anything.
  local out rc
  set +e
  out=$(docker run --rm --network nominatim-itest_default \
        -e POSTGRES_HOST=postgres \
        -e POSTGRES_DB=nominatim_missing \
        -e POSTGRES_SSLMODE=disable \
        -e NOMINATIM_PASSWORD=x \
        -e POSTGRES_ADMIN_PASSWORD=itest-admin-password \
        -e PBF_URL=https://example.invalid/a.pbf \
        "$IMAGE-serve" serve 2>&1)
  rc=$?
  set -e
  if [[ $rc -ne 0 ]] && grep -q 'serve-only' <<<"$out"; then
    ok "serve image refuses to run an import"
  else
    bad "serve image did not refuse the import (rc=$rc): ${out:0:200}"
  fi

  # And it must serve the import the full image created, on a read-only root
  # filesystem, which is how the shipped compose files run it. The project dir
  # is a named volume, not a tmpfs: volumes seed ownership from the image,
  # tmpfs mounts come up root-owned.
  docker rm -f itest-serve >/dev/null 2>&1 || true
  docker run -d --name itest-serve --network nominatim-itest_default \
    --read-only --tmpfs /tmp --tmpfs /var/lib/nominatim \
    -v nominatim-itest-serve:/nominatim \
    --shm-size 256m \
    -e POSTGRES_HOST=postgres \
    -e POSTGRES_DB=nominatim \
    -e POSTGRES_SSLMODE=disable \
    -e NOMINATIM_PASSWORD="${ITEST_PASSWORD:-itest-nominatim-password}" \
    -e GUNICORN_WORKERS=2 \
    -p "127.0.0.1:$((ITEST_PORT + 1)):8080" \
    "$IMAGE-serve" >/dev/null

  local ready=0
  for _ in $(seq 1 60); do
    if curl -fsS --max-time 5 "http://127.0.0.1:$((ITEST_PORT + 1))/status.php?format=json" >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 2
  done
  if [[ $ready -eq 1 ]]; then
    ok "serve image serves the existing import on a read-only root filesystem"
  else
    bad "serve image never became ready"
    docker logs --tail 30 itest-serve || true
  fi
  docker rm -f itest-serve >/dev/null 2>&1 || true
}

# A crashed API must not report success, and a stop must exit 0 promptly.
scenario_shutdown() {
  log "scenario: shutdown semantics"

  local start elapsed
  start=$SECONDS
  $COMPOSE stop -t 60 nominatim >/dev/null
  elapsed=$((SECONDS - start))

  local code
  code=$(docker inspect "$($COMPOSE ps -aq nominatim)" --format '{{.State.ExitCode}}' 2>/dev/null || echo "?")
  if [[ "$code" == "0" ]]; then
    ok "clean stop exits 0"
  else
    bad "clean stop exit code = $code"
  fi
  if ((elapsed < 20)); then
    ok "shutdown completed in ${elapsed}s"
  else
    bad "shutdown took ${elapsed}s"
  fi
}

# A misconfiguration must fail loudly and immediately, not hang.
scenario_failfast() {
  log "scenario: misconfiguration fails fast"

  local out rc
  set +e
  out=$(docker run --rm -e NOMINATIM_PASSWORD= -e PBF_URL=https://example.invalid/a.pbf \
        "$IMAGE" serve 2>&1)
  rc=$?
  set -e
  if [[ $rc -ne 0 ]] && grep -q 'NOMINATIM_PASSWORD' <<<"$out"; then
    ok "missing NOMINATIM_PASSWORD exits non-zero with a clear message"
  else
    bad "missing password did not fail cleanly (rc=$rc): ${out:0:200}"
  fi

  set +e
  out=$(docker run --rm -e NOMINATIM_PASSWORD=x -e UPDATE_MODE=contineous \
        -e PBF_URL=https://example.invalid/a.pbf "$IMAGE" serve 2>&1)
  rc=$?
  set -e
  if [[ $rc -ne 0 ]] && grep -q 'UPDATE_MODE' <<<"$out"; then
    ok "invalid UPDATE_MODE is rejected"
  else
    bad "invalid UPDATE_MODE accepted (rc=$rc): ${out:0:200}"
  fi
}

# A non-ASCII password only authenticates if our SASLprep matches the server's.
# No unit test can establish that; it needs a real PostgreSQL.
scenario_unicode_password() {
  log "scenario: non-ASCII password (SASLprep)"
  cleanup
  # Contains a soft hyphen (mapped away), a no-break space (mapped to a space)
  # and multi-byte characters.
  ITEST_PASSWORD=$'pä\u00adss\u00a0wörd-Ω' $COMPOSE up -d
  if wait_for_api 1200; then
    ok "API authenticates with a non-ASCII password"
    assert_json_nonempty "/search.php?q=avenue%20pasteur" "search works with a non-ASCII password"
  else
    bad "container never came up with a non-ASCII password"
  fi

  # And the cleartext must not be in the logs.
  if $COMPOSE logs nominatim 2>&1 | grep -q 'wörd'; then
    bad "cleartext password appeared in the container log"
  else
    ok "password absent from the container log"
  fi
}

main() {
  local scenarios=("$@")
  if [[ ${#scenarios[@]} -eq 0 ]]; then
    scenarios=(full security restart volume_loss serve_image shutdown failfast unicode_password)
  fi

  build_image

  for s in "${scenarios[@]}"; do
    "scenario_${s//-/_}"
  done

  log "results: $pass passed, $fail failed"
  [[ $fail -eq 0 ]]
}

main "$@"
