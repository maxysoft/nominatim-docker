#!/usr/bin/env bash

set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE="docker compose -f test/docker-compose.test.yml"
ITEST_PORT="${ITEST_PORT:-18080}"
export ITEST_PORT
BASE_URL="http://127.0.0.1:${ITEST_PORT}"
SPLIT="docker compose -f contrib/docker-compose-local.yml -p nominatim-itest-split --profile updates"
# Exported once so the EXIT trap can interpolate the compose file too.
export NOMINATIM_PORT=$((ITEST_PORT + 2)) NOMINATIM_PASSWORD=itest-nominatim-password NOMINATIM_WEBUSER_PASSWORD=itest-web-password POSTGRES_ADMIN_PASSWORD=itest-admin-password
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
  $SPLIT down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

wait_for_api() {
  local deadline=$((SECONDS + ${1:-900}))
  while ((SECONDS < deadline)); do
    if ! service_running main nominatim; then
      log "nominatim container is no longer running"
      $COMPOSE logs --tail 60 nominatim
      return 1
    fi
    if status_ok "$BASE_URL/status.php?format=json"; then
      return 0
    fi
    sleep 5
  done
  log "timed out waiting for the API"
  $COMPOSE logs --tail 60 nominatim
  return 1
}

# The HTTP code alone proves nothing: Nominatim answers 200 {"status":700} when it cannot reach the DB.
status_ok() {
  local body
  body=$(curl -fsS --max-time 5 "$1" 2>/dev/null) && [[ $body == *'"status":0'* ]]
}

wait_url() {
  for _ in $(seq 1 "${2:-60}"); do
    status_ok "$1" && return 0
    sleep 2
  done
  return 1
}

wait_for_exit() {
  [[ -z "$1" ]] && { echo missing; return; }
  timeout "$2" docker wait "$1" 2>/dev/null || echo timeout
}

# Captures first: piping into grep -q trips pipefail (SIGPIPE, rc 141).
logs_contain() {
  local out
  case $1 in
    main) out=$($COMPOSE logs "$2" 2>/dev/null) ;;
    split) out=$($SPLIT logs "$2" 2>/dev/null) ;;
  esac
  grep -q -- "$3" <<<"$out"
}

# Captures first, for the same pipefail reason as logs_contain.
service_running() {
  local out
  case $1 in
    main) out=$($COMPOSE ps --status running --services 2>/dev/null || true) ;;
    split) out=$($SPLIT ps --status running --services 2>/dev/null || true) ;;
  esac
  grep -qx -- "$2" <<<"$out"
}

assert_json_nonempty() {
  local body
  body=$(curl -fsS --max-time 30 "$BASE_URL$1" 2>/dev/null || true)
  if [[ -n "$body" && "$body" != "[]" && "$body" != "{}" ]]; then
    ok "$2"
  else
    bad "$2 (got: ${body:0:120})"
  fi
}

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

  # GET, not HEAD: Nominatim answers HEAD with a 405 whose error body is JSON too.
  local ct
  ct=$(curl -s -o /dev/null -w '%{content_type}' --max-time 15 "$BASE_URL/status.php?format=json" || true)
  if [[ $ct == application/json* ]]; then
    ok "Content-Type is application/json"
  else
    bad "Content-Type is not application/json (got: $ct)"
  fi

  local check
  # shellcheck disable=SC2016
  if check=$($COMPOSE exec -T -u nominatim nominatim sh -c \
    'NOMINATIM_DATABASE_DSN="pgsql:host=$POSTGRES_HOST;dbname=$POSTGRES_DB;user=nominatim;password=$NOMINATIM_PASSWORD" exec nominatim admin --check-database --project-dir /nominatim' 2>&1); then
    ok "nominatim admin --check-database succeeds"
  else
    bad "nominatim admin --check-database failed"
    printf '%s\n' "$check" | tail -n 15
  fi
}

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

  # Check setgid as well as setuid: the Dockerfile strips both.
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

  # Read the UID from /proc: docker top resolves UIDs against the host passwd file.
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

  # Poll: docker compose logs lags briefly behind the container writing a line.
  local found=0
  for _ in $(seq 1 30); do
    if logs_contain main nominatim 'skipping import'; then
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

  $COMPOSE exec -T postgres psql -U postgres -qc "COMMENT ON DATABASE nominatim IS NULL" >/dev/null
  local out rc marker
  set +e
  out=$($COMPOSE run --rm --no-deps -T -e ALLOW_DROP_EXISTING_DB=true nominatim import 2>&1)
  rc=$?
  set -e
  marker=$($COMPOSE exec -T postgres psql -U postgres -tAc \
    "SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname='nominatim'" 2>/dev/null || true)
  after=$($COMPOSE exec -T postgres psql -U postgres -d nominatim -tAc \
    "SELECT count(*) FROM placex" 2>/dev/null | tr -d '[:space:]' || true)
  if [[ $rc -eq 0 && $marker == 'nominatim-docker: import complete' && $after == "$before" ]] &&
      grep -q 'adopting the existing import' <<<"$out"; then
    ok "unstamped import adopted, not dropped, under ALLOW_DROP_EXISTING_DB"
  else
    bad "unstamped import not adopted (rc=$rc, marker=$marker, placex $before -> $after): ${out: -300}"
  fi
}

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

scenario_serve_image() {
  log "scenario: serve-only image"
  if ! service_running main nominatim; then
    log "starting the import stack this scenario runs against"
    cleanup
    $COMPOSE up -d
    wait_for_api 1200 || { bad "API never became ready"; return; }
  fi

  log "building $IMAGE-serve"
  DOCKER_BUILDKIT=1 docker build --target serve -t "$IMAGE-serve" .

  for binary in osm2pgsql psql; do
    if docker run --rm --entrypoint sh "$IMAGE-serve" -c "command -v $binary" >/dev/null 2>&1; then
      bad "$binary is still present in the serve image"
    else
      ok "$binary absent from the serve image"
    fi
  done

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

  # The project dir is a named volume, not a tmpfs: volumes seed ownership from the image, tmpfs mounts come up root-owned.
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

  if wait_url "http://127.0.0.1:$((ITEST_PORT + 1))/status.php?format=json"; then
    ok "serve image serves the existing import on a read-only root filesystem"
  else
    bad "serve image never became ready"
    docker logs --tail 30 itest-serve || true
  fi
  docker rm -f itest-serve >/dev/null 2>&1 || true
}

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

  $COMPOSE up -d postgres >/dev/null 2>&1
  set +e
  out=$(timeout 90 docker run --rm --network nominatim-itest_default \
        -e POSTGRES_HOST=postgres -e POSTGRES_SSLMODE=disable \
        -e NOMINATIM_PASSWORD=x -e POSTGRES_ADMIN_PASSWORD=wrong-password \
        -e PBF_URL=https://example.invalid/a.pbf "$IMAGE" serve 2>&1)
  rc=$?
  set -e
  if [[ $rc -ne 0 && $rc -ne 124 ]] && grep -q 'rejected the credentials' <<<"$out"; then
    ok "wrong database password fails fast"
  else
    bad "wrong password did not fail fast (rc=$rc): ${out:0:200}"
  fi
}

scenario_unicode_password() {
  log "scenario: non-ASCII password (SASLprep)"
  cleanup
  ITEST_PASSWORD=$'pä\u00adss\u00a0wörd-Ω' $COMPOSE up -d
  if wait_for_api 1200; then
    ok "API authenticates with a non-ASCII password"
    assert_json_nonempty "/search.php?q=avenue%20pasteur" "search works with a non-ASCII password"
  else
    bad "container never came up with a non-ASCII password"
  fi

  if logs_contain main nominatim 'wörd'; then
    bad "cleartext password appeared in the container log"
  else
    ok "password absent from the container log"
  fi
  cleanup
}

scenario_drop_guard() {
  log "scenario: drop guard"
  cleanup
  $COMPOSE up -d postgres >/dev/null 2>&1
  # Over TCP: the image's temporary init server listens on the socket only, so this reaches the final server.
  pg() { $COMPOSE exec -T postgres psql -h 127.0.0.1 -U postgres -v ON_ERROR_STOP=1 -qtA "$@"; }
  for _ in $(seq 60); do pg -c 'SELECT 1' >/dev/null 2>&1 && break; sleep 2; done
  pg -c 'SELECT 1' >/dev/null 2>&1 || { bad "postgres never became ready for drop_guard"; return; }
  pg -c "CREATE DATABASE guarded" >/dev/null
  pg -d guarded -c "CREATE TABLE precious (id int); INSERT INTO precious VALUES (1)" >/dev/null

  local out rc
  set +e
  out=$(timeout 300 docker run --rm --network nominatim-itest_default \
        -e POSTGRES_HOST=postgres -e POSTGRES_DB=guarded -e POSTGRES_SSLMODE=disable \
        -e NOMINATIM_PASSWORD=itest-nominatim-password -e POSTGRES_ADMIN_PASSWORD=itest-admin-password \
        -e PBF_URL=https://example.invalid/a.pbf "$IMAGE" serve 2>&1)
  rc=$?
  set -e
  if [[ $rc -ne 0 && $rc -ne 124 ]] && grep -q 'holds tables but no completion marker' <<<"$out"; then
    ok "database with tables of its own is refused"
  else
    bad "drop guard did not refuse (rc=$rc): ${out:0:300}"
  fi
  if [[ "$(pg -d guarded -c 'SELECT count(*) FROM precious' 2>/dev/null)" == 1 ]]; then
    ok "its data survived"
  else
    bad "its data is gone"
  fi

  pg -c "REVOKE CONNECT ON DATABASE guarded FROM PUBLIC" >/dev/null
  set +e
  out=$(timeout 300 docker run --rm --network nominatim-itest_default \
        -e POSTGRES_HOST=postgres -e POSTGRES_DB=guarded -e POSTGRES_SSLMODE=disable -e ALLOW_DROP_EXISTING_DB=true \
        -e NOMINATIM_PASSWORD=itest-nominatim-password -e POSTGRES_ADMIN_PASSWORD=itest-admin-password \
        -e PBF_URL=https://example.invalid/a.pbf "$IMAGE" serve 2>&1)
  rc=$?
  set -e
  if [[ $rc -ne 0 && $rc -ne 124 ]] && grep -q 'nominatim role cannot connect' <<<"$out" &&
      [[ "$(pg -d guarded -c 'SELECT count(*) FROM precious' 2>/dev/null)" == 1 ]]; then
    ok "unvalidatable database kept under ALLOW_DROP_EXISTING_DB"
  else
    bad "unvalidatable database not refused (rc=$rc): ${out: -300}"
  fi

  # The key must match importLockClass in internal/ctl/db.go.
  pg -c "SELECT pg_advisory_lock(1313820489, hashtext('guarded'))" -c "SELECT pg_sleep(30)" >/dev/null 2>&1 &
  for _ in $(seq 20); do
    [[ "$(pg -c "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted" 2>/dev/null)" == 1 ]] && break
    sleep 1
  done
  set +e
  out=$(timeout 15 docker run --rm --network nominatim-itest_default \
        -e POSTGRES_HOST=postgres -e POSTGRES_DB=guarded -e POSTGRES_SSLMODE=disable \
        -e NOMINATIM_PASSWORD=itest-nominatim-password -e POSTGRES_ADMIN_PASSWORD=itest-admin-password \
        -e PBF_URL=https://example.invalid/a.pbf "$IMAGE" serve 2>&1)
  rc=$?
  set -e
  if [[ $rc -eq 124 ]] && grep -q 'waiting for another import into' <<<"$out" &&
      ! grep -q 'completion marker' <<<"$out"; then
    ok "a second importer waits for the import lock"
  else
    bad "import lock not honoured (rc=$rc): ${out:0:300}"
  fi
  wait
}

scenario_split() {
  log "scenario: split deployment (contrib/docker-compose-local.yml)"
  $SPLIT down --volumes --remove-orphans >/dev/null 2>&1 || true
  local url="http://127.0.0.1:${NOMINATIM_PORT}"

  local out code
  if ! out=$($SPLIT up -d --build nominatim-import 2>&1); then
    bad "compose could not start the import service: ${out: -300}"
    return
  fi
  code=$(wait_for_exit "$($SPLIT ps -aq nominatim-import)" 1500)
  if [[ "$code" == "0" ]] && logs_contain split nominatim-import 'running import'; then
    ok "one-shot import exited 0 after importing"
  else
    bad "import container exit code $code"
    $SPLIT logs --tail 40 nominatim-import
    return
  fi
  $SPLIT up -d nominatim >/dev/null 2>&1 || true

  if wait_url "$url/status.php?format=json"; then
    ok "serve-only API answers"
  else
    bad "serve-only API never became ready"
    $SPLIT logs --tail 30 nominatim
    return
  fi

  if $SPLIT exec -T nominatim sh -c 'command -v osm2pgsql || command -v psql' >/dev/null 2>&1; then
    bad "API container ships import tooling"
  else
    ok "API container has no osm2pgsql or psql"
  fi
  # shellcheck disable=SC2016  # must expand in the container's shell, not here
  if $SPLIT exec -T nominatim sh -c 'test -z "$POSTGRES_ADMIN_PASSWORD"'; then
    ok "API container holds no admin password"
  else
    bad "POSTGRES_ADMIN_PASSWORD present in the API container"
  fi

  local before after
  before=$($SPLIT exec -T nominatim-postgres psql -U postgres -d nominatim -tAc "SELECT count(*) FROM placex" 2>/dev/null | tr -d '[:space:]' || true)
  $SPLIT up -d nominatim-import >/dev/null 2>&1 || true
  code=$(wait_for_exit "$($SPLIT ps -aq nominatim-import)" 300)
  if [[ "$code" == "0" ]] && logs_contain split nominatim-import 'skipping import'; then
    ok "re-running the import service skips the completed import"
  else
    bad "second import run did not skip (exit $code)"
    $SPLIT logs --tail 20 nominatim-import
  fi
  after=$($SPLIT exec -T nominatim-postgres psql -U postgres -d nominatim -tAc "SELECT count(*) FROM placex" 2>/dev/null | tr -d '[:space:]' || true)
  if [[ -n "$before" && "$before" == "$after" ]]; then
    ok "database untouched by the re-run ($before rows)"
  else
    bad "placex changed across the re-run: $before -> $after"
  fi

  $SPLIT up -d nominatim-updater >/dev/null 2>&1 || true
  local started=0
  for _ in $(seq 1 45); do
    if logs_contain split nominatim-updater 'starting replication'; then started=1; break; fi
    sleep 2
  done
  if [[ $started -eq 1 ]] && service_running split nominatim-updater; then
    ok "updater initialised replication and is running"
  else
    bad "updater did not start replication"
    $SPLIT logs --tail 30 nominatim-updater
  fi

  $SPLIT down --volumes --remove-orphans >/dev/null 2>&1 || true
}

main() {
  local scenarios=("$@")
  if [[ ${#scenarios[@]} -eq 0 ]]; then
    scenarios=(full security restart volume_loss serve_image shutdown failfast unicode_password split drop_guard)
  fi

  build_image

  for s in "${scenarios[@]}"; do
    "scenario_${s//-/_}"
  done

  log "results: $pass passed, $fail failed"
  [[ $fail -eq 0 ]]
}

main "$@"
