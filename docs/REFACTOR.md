# Refactor: shell entrypoint → `nominatim-ctl`

This branch replaces the three shell scripts that drove the container
(`config.sh`, `init.sh`, `start.sh` — 439 lines) with a single static Go binary,
and rebuilds the image around it. Nominatim itself is unchanged: it is a Python
application and stays one.

The externally visible contract is preserved. Every environment variable, every
lifecycle step, and every assertion in `.github/workflows/ci.yml` still applies,
except where listed under [Breaking changes](#breaking-changes).

Two CI assertions were changed rather than preserved: the `UPDATE_MODE` scenarios
grepped `/var/log/replication.log`, and replication output now goes to the
container's stdout, so they read `docker logs` instead.

---

## Why not just fix the shell

The scripts had four defects that were structural rather than incidental, and
each of them disappears as a category once configuration is a typed value
rendered from scratch on every boot:

- **`config.sh` had no shebang.** Executing it produced `ENOEXEC`; bash re-ran it
  in a fallback shell that does **not** inherit `errexit`. All eight `sed -i`
  calls could fail with rc=2 and be silently ignored, and the script was
  structurally incapable of reporting failure.
- **Templating was one-shot.** `.env` was baked into the image at
  `$PROJECT_DIR`, which is normally a volume. The `__PLACEHOLDER__` tokens were
  consumed on the first run, so from the second boot onwards `POSTGRES_HOST`,
  `NOMINATIM_PASSWORD`, `IMPORT_STYLE` and `REPLICATION_URL` were silently
  ignored. Rotating the database password made the container loop forever on
  "PostgreSQL is unavailable" with no diagnostic.
- **The interval `sed` corrupted its own output.** The pattern was the literal
  default and unanchored, so `REPLICATION_UPDATE_INTERVAL=864000` followed by
  `300` produced `3000`. Measured, not theorised.
- **The import guard lived in the wrong volume.** `DROP DATABASE IF EXISTS` was
  gated on `$PROJECT_DIR/import-finished`, a file in the *application* volume,
  while the data lived in the *database* volume. Removing or renaming the
  application volume dropped a fully populated database with no prompt.

A bind-mounted `PROJECT_DIR` — the documented planet deployment — made these
compound: Docker never seeds a bind mount from the image, so `.env` did not
exist, every `sed` failed against a missing file, `config.sh` exited 0 anyway,
and the container silently fell back to Nominatim's built-in DSN pointing at a
local socket that does not exist in this image.

---

## What the binary does differently

| Concern | Before | Now |
| --- | --- | --- |
| Configuration | 8 in-place `sed` substitutions on a persisted file | Struct → file, fully regenerated every boot, `0600` |
| Import marker | File in a different volume from the data | A `COMMENT ON DATABASE` written only after the import succeeds |
| Privilege drop | `sudo -E -u nominatim` (setuid-root binary in the image) | `SysProcAttr.Credential` — a direct fork+setuid, no setuid binary anywhere |
| Database access | 9 `psql -c "…${VAR}…"` string interpolations | `pgx` with bind parameters; quoted identifiers and literals where PostgreSQL forbids parameters |
| Supplementary data | `sshpass -p <password> scp -o StrictHostKeyChecking=no` | HTTPS against the system CA bundle, optional SHA-256 |
| API supervision | `--daemon` + PID-file polling + unconditional `exit 0` | Foreground child, `cmd.Wait()`, real exit code propagated |
| Shutdown | Trap deferred behind `sleep 5`; kill then immediate exit | Signal wakes the select immediately; SIGTERM, drain, escalate to SIGKILL at 35 s |
| Worker sizing | `nproc` (blind to the CFS quota) | `/sys/fs/cgroup/cpu.max`, falling back to `NumCPU` |
| Zombie reaping | Incidental, via bash's SIGCHLD handling | Delegated to the runtime (`init: true` / `docker run --init`); see note below |
| Failure modes | Three unbounded `until` loops with stderr discarded | Bounded retries that report the real driver error and exit non-zero |

`config.sh` + `init.sh` + `start.sh` (439 lines of bash, ~20% of it duplicated
verbatim between two files) become 1697 lines of Go — 3.5× more code —
plus 27 unit tests (29 cases including subtests). The line count goes up substantially; what goes down is
the number of ways to be silently wrong. That trade is the whole argument, and
it is worth stating honestly rather than dressing up as a reduction.

**On reaping.** An earlier draft of this refactor ran a `Wait4(-1, WNOHANG)`
reaper when PID 1. That is unsafe in Go: it races `exec.Cmd.Wait` for the exit
status of the orchestrator's *own* children, and whichever call loses gets
`ECHILD`. The visible symptom would be an intermittent "import failed" on an
import that actually succeeded — strictly worse than the zombies it prevents.
Reaping is therefore left to the container runtime; every compose file in
`contrib/` sets `init: true`, and the entrypoint logs a note if it finds itself
at PID 1 without one.

---

### Measured

Both images built on the same host from the same base digest:

| | `master` | this branch |
| --- | --- | --- |
| Image size | 1.1 GB | 739 MB |
| setuid/setgid binaries | 14 | 0 |
| Shutdown latency (`docker stop`) | up to 5 s | ~1 s |
| Python dependencies pinned | 1 of 6 | 24 of 24, with hashes |

BuildKit's own linter flags `master` with
`SecretsUsedInArgOrEnv: Do not use ARG or ENV instructions for sensitive data
(ENV "NOMINATIM_PASSWORD")`, which is the same finding as C1 below.

The size reduction comes from dropping `build-essential`, `python3-dev`,
`libicu-dev`, `pkg-config`, `sudo`, `sshpass`, `openssh-client` and `curl` from
the shipped image, and from replacing the `FROM scratch` + `COPY --from=build / /`
layer collapse with a real multi-stage build. The collapse traded away the base
image's `ENV` (including `LANG`), any `LABEL`/`HEALTHCHECK`, base-image
provenance for scanners, and all layer sharing between versions — for a saving
that a normal multi-stage build exceeds anyway.

## Security changes

Ordered by severity, using the identifiers from the audit that produced them.

**C1 — a hardcoded password became a PostgreSQL superuser password.**
`ENV NOMINATIM_PASSWORD=qaIACxO6wMR3` shipped in the image, was documented in
three files, and is in git history. `init.sh` defaulted
`POSTGRES_ADMIN_PASSWORD` to it and then ran
`CREATE USER nominatim SUPERUSER`. Removed: the variable is now required, has no
default, and supports `NOMINATIM_PASSWORD_FILE`. The admin password is never
derived from it.

**C2 — the internet-facing API held a superuser connection.** Gunicorn read the
project `.env`, whose DSN named the superuser role. Any flaw in `nominatim-api`
escalated to full cluster compromise. The API now connects as the read-only
`www-data` role that the previous code created, gave a password, and never used.
The application role drops from `SUPERUSER` to `CREATEDB`. Installing PostGIS,
`postgis_raster` and `hstore` — the only superuser-requiring step — is done by the
administrative connection into `template1`. To be precise about what this buys:
the superuser requirement is **relocated, not eliminated** (`POSTGRES_ADMIN_PASSWORD`
is still needed for the initial import). The win is that no long-lived connection,
and in particular not the internet-facing one, is a superuser. The cost is that
`PROVISION_EXTENSIONS=true` mutates `template1` cluster-wide, so every database
later created on that server inherits PostGIS; set `PROVISION_EXTENSIONS=false`
on a shared cluster and install the extensions yourself.

**C3 — unauthenticated fetch of a SQL dump that is then executed.**
`StrictHostKeyChecking=no` removed the only authentication of the storage host,
and `wikimedia-secondary-importance.sql.gz` is loaded into the database. Anyone
able to spoof that host could execute arbitrary SQL as a superuser. All five
datasets are now fetched over HTTPS from `https://nominatim.org/data` (verified
reachable), with an optional per-dataset `*_SHA256`. `sshpass` and
`openssh-client` are gone, along with the committed storage credentials — which
are in public git history and should be considered permanently burned.

**C4 — unguarded `DROP DATABASE`.** Now refuses to drop a database containing
`public.placex` unless `ALLOW_DROP_EXISTING_DB=true`.

**C5 — SQL injection via `NOMINATIM_PASSWORD` and `POSTGRES_DB`.** A password
containing `'` broke the statement; a crafted one executed as superuser. An
unquoted `${POSTGRES_DB}` both folded case and allowed statement chaining. Both
are quoted now, with unit tests covering the injection strings.

**Role hijacking.** `ALTER USER … PASSWORD` ran unconditionally, so starting this
container against a shared cluster silently reset the password of any
pre-existing `www-data` role. Managed roles are now tagged with
`COMMENT ON ROLE`; an untagged role is left alone and the container stops with
an explanation.

**Also addressed:** TLS to the database is configurable via `POSTGRES_SSLMODE`
(present in the DSN and every driver connection); secrets are redacted from
everything the entrypoint itself logs, so `DEBUG_MODE` no longer echoes passwords
— though child output (Gunicorn, osm2pgsql, Python tracebacks) passes through
unfiltered, and `ALTER ROLE … PASSWORD` still reaches the PostgreSQL server log
when `log_statement` is `ddl` or `all`; `.env` is chmod-ed to `0600` on every
write, including one left at `0644` on a volume by an older image; the web role
has its own `NOMINATIM_WEBUSER_PASSWORD` so leaking the API's DSN does not also
hand over the `CREATEDB` role; every setuid and setgid bit in the image is stripped at build time,
which makes `no-new-privileges:true` meaningful; Python dependencies are pinned
with hashes (`gunicorn>=25.0` was silently resolving to 26.0.0); GitHub Actions
are pinned to commit SHAs and the workflow has a least-privilege
`permissions:` block.

---

## Environment variable contract

**Unchanged:** `PBF_URL`, `PBF_PATH`, `REPLICATION_URL`,
`REPLICATION_UPDATE_INTERVAL`, `REPLICATION_RECHECK_INTERVAL`, `UPDATE_MODE`,
`FREEZE`, `REVERSE_ONLY`, `IMPORT_WIKIPEDIA`, `IMPORT_SECONDARY_WIKIPEDIA`,
`IMPORT_GB_POSTCODES`, `IMPORT_US_POSTCODES`, `IMPORT_TIGER_ADDRESSES`,
`IMPORT_STYLE`, `THREADS`, `GUNICORN_WORKERS`, `POSTGRES_HOST`, `POSTGRES_PORT`,
`POSTGRES_DB`, `NOMINATIM_PASSWORD`, `POSTGRES_ADMIN_PASSWORD`,
`WARMUP_ON_STARTUP`, `DEBUG_MODE`, `PROJECT_DIR`, `USER_AGENT`.

**Added**

| Variable | Default | Purpose |
| --- | --- | --- |
| `NOMINATIM_PASSWORD_FILE`, `POSTGRES_ADMIN_PASSWORD_FILE` | unset | Read the secret from a file instead of the environment |
| `POSTGRES_SSLMODE` | `prefer` | Any libpq sslmode; use `require` or stricter in production |
| `DATA_MIRROR_URL` | `https://nominatim.org/data` | Base URL for supplementary datasets |
| `IMPORT_*_SHA256`, `PBF_SHA256` | unset | Verify a download before it is used |
| `ALLOW_DROP_EXISTING_DB` | `false` | Permit overwriting a populated database |
| `NOMINATIM_ROLE_OPTIONS` | `CREATEDB` | Role attributes; set to `SUPERUSER` to restore the old behaviour |
| `PROVISION_EXTENSIONS` | `true` | Install PostGIS and hstore into `template1` |
| `API_DB_USER` | `www-data` | Database role the API connects as |
| `GUNICORN_BIND` | `0.0.0.0:8080` | Bind address |
| `GUNICORN_TIMEOUT`, `GUNICORN_GRACEFUL_TIMEOUT` | `60`, `30` | Request and drain deadlines |
| `POSTGRES_ADMIN_USER` | `postgres` | Administrative role name |
| `NOMINATIM_WEBUSER` | `www-data` | Read-only role name |

**Removed:** `STORAGE_USER`, `STORAGE_HOST`, `STORAGE_PASSWORD` — superseded by
`DATA_MIRROR_URL` now that the transport is HTTPS.

---

## Breaking changes

1. **`NOMINATIM_PASSWORD` is required.** No default ships. The container exits
   non-zero with a clear message instead of using a published password.
2. **`POSTGRES_ADMIN_PASSWORD` is required for the initial import** and is no
   longer derived from `NOMINATIM_PASSWORD`.
3. **`sudo` is not in the image.** Replace
   `docker exec … sudo -u nominatim nominatim …` with
   `docker exec -u nominatim … nominatim …`. Already updated in `ci.yml` and
   `howto.md`.
4. **`.env` is regenerated on every start.** Hand edits to
   `$PROJECT_DIR/.env` will not survive. Use environment variables.
5. **Import is detected from the database, not `import-finished`.** A container
   pointed at a populated database will skip the import even on a fresh volume —
   which is the fix for the data-loss path, but it does mean re-importing into
   the same database now requires `ALLOW_DROP_EXISTING_DB=true`.
6. **Dataset paths must be absolute.** `IMPORT_WIKIPEDIA=data/wiki.csv.gz`
   silently resolved against `/app` and was skipped; it is now rejected.
7. **`UPDATE_MODE` is validated.** A typo used to mean "no replication",
   silently. It now fails at startup.
8. **A crashed Gunicorn exits non-zero.** The old code always exited 0, so
   orchestrators with `restartPolicy: OnFailure` never restarted a dead API.
   A signalled shutdown still exits 0 at any point in the lifecycle — the handler
   is installed in `main` before any long-running work, so stopping the container
   mid-import is a clean exit rather than a crash.
9. **The application role is no longer a superuser.** If your provider cannot
   install extensions into `template1`, set `PROVISION_EXTENSIONS=false` and
   install PostGIS yourself, or set `NOMINATIM_ROLE_OPTIONS=SUPERUSER`.

### Migrating an existing deployment

```bash
# 1. Set the two passwords explicitly — there is no default any more.
cp contrib/.env.example contrib/.env && $EDITOR contrib/.env

# 2. Downgrade the application role and install the extensions centrally.
psql -h "$POSTGRES_HOST" -U postgres -c 'ALTER ROLE nominatim NOSUPERUSER CREATEDB;'
psql -h "$POSTGRES_HOST" -U postgres -d template1 \
     -c 'CREATE EXTENSION IF NOT EXISTS postgis; CREATE EXTENSION IF NOT EXISTS hstore;'

# 3. Adopt the roles this image now manages, so it may rotate their passwords.
psql -h "$POSTGRES_HOST" -U postgres \
     -c "COMMENT ON ROLE nominatim IS 'managed by nominatim-docker';" \
     -c "COMMENT ON ROLE \"www-data\" IS 'managed by nominatim-docker';"
```

The existing data volume and database are reused; no re-import is needed.

---

## Testing

```bash
make check        # go vet + unit tests, in a container
make lint         # hadolint + shellcheck
make build        # build the image
make integration  # full local stack: import Monaco, assert behaviour
```

`test/integration.sh` runs against a real PostGIS container. It covers the API
surface and the regressions this refactor targets — **not** the whole CI matrix.
Not covered locally, and still only exercised in CI: `UPDATE_MODE=once`/
`continuous`, `FREEZE`, `PBF_PATH`, the GB postcode import, `WARMUP_ON_STARTUP`,
the dataset download path, the flatnode directory, `*_SHA256` verification, the
role-adoption refusal, and the `ALLOW_DROP_EXISTING_DB` guard. The scenarios it
does run:

- `full` — import, then the search/reverse/lookup/details/status surface and
  `nominatim admin --check-database`
- `security` — the application role is not a superuser, the API is connected as
  `www-data`, no setuid binaries, `sudo`/`sshpass`/`ssh`/`curl` absent, `.env` is
  `0600` and placeholder-free, Gunicorn runs unprivileged
- `restart` — `placex` row count is unchanged across a restart
- `volume_loss` — **removing the project volume does not drop the database**
- `shutdown` — clean stop exits 0, promptly
- `failfast` — misconfiguration exits non-zero with a diagnostic instead of
  hanging

Unit tests cover the pure logic where the shell bugs lived: SQL quoting against
the injection strings, `.env` rendering idempotence and the interval-splicing
bug, cgroup CPU parsing, secret redaction, and configuration validation.

---

## Known gaps

Deliberately not addressed, listed so they are not mistaken for oversights:

- **The role password still reaches the PostgreSQL server log.** `ALTER ROLE …
  PASSWORD '<literal>'` is logged verbatim when `log_statement` is `ddl` or `all`.
  Closing this needs a pre-computed SCRAM-SHA-256 verifier.
- **Child process output is not redacted.** Only what the entrypoint itself logs
  passes through the filter; a Python traceback from Nominatim could still print a
  DSN.
- **`PROVISION_EXTENSIONS` defaults to `true`**, which mutates `template1`
  cluster-wide on first import. Convenient for a dedicated server, wrong for a
  shared one — set it to `false` there.
- **`PyICU` is source-only on PyPI**, so the `linux/arm64` leg of the publish job
  compiles a C++ extension under QEMU and is slow. `master` used Debian's prebuilt
  `python3-icu`.
- **A `/nominatim` volume written by the old image may contain root-owned files.**
  `chownProjectFiles` is deliberately non-recursive so it cannot clobber operator
  bind mounts, which means it also cannot repair those. Run
  `chown -R 1000:1000` on the volume once if the container reports permission errors.
- **Two further undeclared changes to defaults:** a non-integer
  `REPLICATION_UPDATE_INTERVAL`/`RECHECK_INTERVAL` is now a startup error rather
  than being ignored; and Gunicorn gains `--max-requests 10000` worker recycling
  and a `/dev/shm` worker temp dir. All three are overridable
  (`GUNICORN_MAX_REQUESTS`, `GUNICORN_WORKER_TMP_DIR`, `GUNICORN_LIMIT_REQUEST_FIELDS`).
- **The CI matrix has not been executed against this branch** — only the local
  suite. Eight of the sixteen scenarios exercise paths the local suite does not.

## Not changed

- Nominatim itself, its version (5.3.2), and its Python stack.
- The HTTP API surface, port, and response formats.
- `contrib/postgres/*.conf` tuning profiles.
- The Varnish setup, beyond two pre-existing bugs fixed in
  `contrib/docker-compose-varnish.yml`: a missing top-level `networks:` block that
  made it fail `docker compose config` on master, and `POSTGRES_HOST: postgres`,
  which named a service that does not exist in that file (it is
  `nominatim-postgres`), so the stack could parse and still never start.
- The publish job still rebuilds the image rather than promoting the tested
  artifact, because the test matrix builds `linux/amd64` only while publish
  builds `amd64,arm64`. Provenance and SBOM attestation were added; closing the
  gap properly needs a digest-based promotion step and is left as follow-up.
