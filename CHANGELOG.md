# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Releases

### Unreleased: entrypoint rewritten in Go

Full rationale, parity matrix and migration steps: [docs/REFACTOR.md](docs/REFACTOR.md).

- **Changed:** Replaced `config.sh`, `init.sh` and `start.sh` (439 lines of bash) with `nominatim-ctl`,
  a static Go binary that runs as PID 1. Nominatim itself is unchanged.
- **Changed:** `.env` is regenerated in full on every start instead of being patched with `sed`.
  Configuration changes now take effect on restart; previously the `__PLACEHOLDER__` tokens were
  consumed on the first run and every later start silently ignored `POSTGRES_HOST`,
  `NOMINATIM_PASSWORD`, `IMPORT_STYLE` and `REPLICATION_URL`.
- **Changed:** Import completion is detected from the database (`public.placex`) rather than the
  `import-finished` file, which lived in a different volume from the data it guarded.
- **Changed:** The image runs Gunicorn in the foreground; a crash now exits non-zero. A signalled
  shutdown still exits 0.
- **Changed:** Supplementary datasets are fetched over HTTPS from `nominatim.org` instead of `scp`.
- **Changed:** The `nominatim` database role is created with `CREATEDB` instead of `SUPERUSER`;
  PostGIS is installed into `template1` by the administrative connection.
- **Changed:** The API connects as the read-only `www-data` role.
- **Security:** Removed the hardcoded `NOMINATIM_PASSWORD` default, which became a PostgreSQL
  superuser password. The variable is now required.
- **Security:** Removed `sudo`, `sshpass` and `openssh-client`; all setuid/setgid bits are stripped
  at build time, so `no-new-privileges:true` is now meaningful.
- **Security:** Fixed SQL injection through `NOMINATIM_PASSWORD` and `POSTGRES_DB`.
- **Security:** `DROP DATABASE` refuses to touch a populated database without `ALLOW_DROP_EXISTING_DB=true`.
- **Security:** Pinned all Python dependencies with hashes (`gunicorn>=25.0` was resolving to 26.0.0)
  and all GitHub Actions to commit SHAs; added a least-privilege `permissions:` block to CI.
- **Added:** `POSTGRES_SSLMODE`, `DATA_MIRROR_URL`, `ALLOW_DROP_EXISTING_DB`, `NOMINATIM_WEBUSER`,
  `GUNICORN_BIND`, `NOMINATIM_ROLE_OPTIONS`, `PROVISION_EXTENSIONS`, `*_SHA256` checksums, and
  `_FILE` variants for both passwords.
- **Added:** A `HEALTHCHECK` hitting `/status.php`, implemented in the entrypoint so the image needs no curl.
- **Added:** `make check` / `make integration` and `test/integration.sh`, a local stack that imports
  Monaco and asserts the API surface, privilege model, restart behaviour and shutdown semantics.
- **Fixed:** Worker and thread counts respect the container CPU quota instead of reading host core count.
- **Fixed:** `contrib/docker-compose-varnish.yml` was missing its top-level `networks:` block and
  failed `docker compose config`.
- **Removed:** `STORAGE_USER`, `STORAGE_HOST`, `STORAGE_PASSWORD`.

Follow-up hardening (closes the remaining documented gaps):

- **Security:** Role passwords are set with a client-computed SCRAM-SHA-256 verifier instead of
  `ALTER ROLE ... PASSWORD '<cleartext>'`, which the server records verbatim under
  `log_statement=ddl|all`. RFC 4013 SASLprep is applied first, so this covers non-ASCII passwords
  too, with a fallback to the raw bytes that mirrors the server.
- **Security:** Child process output (Gunicorn, osm2pgsql, Python tracebacks) is now filtered through
  the secret masker as well, line by line. Previously only the entrypoint's own logging was redacted,
  so a traceback could still print a DSN.
- **Changed:** `template1` is modified only when an extension is genuinely missing, is skipped
  entirely for a superuser role, and is logged when it happens. Nominatim's `createdb` fails on an
  existing database, so `template1` remains the only route for an unprivileged role.
- **Changed:** PyICU comes from Debian's prebuilt `python3-icu` via a `--system-site-packages` venv
  rather than being compiled from an sdist. It has no wheel, so the arm64 publish leg was compiling
  a C++ extension under QEMU. Every other dependency is a wheel, so the build stage no longer
  installs a compiler at all.
- **Docs:** Three previously undeclared behaviour changes are now in Breaking changes: a
  non-integer replication interval is a startup error, the new Gunicorn defaults, and the
  `template1` modification.
- **Test:** An integration scenario imports with a non-ASCII password containing a soft hyphen and a
  no-break space, proving the SASLprep path against a real PostgreSQL, and asserts the cleartext
  does not reach the container log.

Fixes from an independent review of the refactor itself:

- **Fixed:** `cap_drop: ALL` in the `contrib/` compose files omitted `CAP_KILL`, so the entrypoint
  (uid 0) could not signal the Gunicorn process it forked as uid 1000. Graceful shutdown silently
  failed and the container was SIGKILLed at the stop timeout. The test stack now mirrors the shipped
  capability set so this cannot regress unnoticed.
- **Fixed:** The import/skip decision was made before the database was known to be reachable, so a
  restart during a brief PostgreSQL blip took the import branch and exited non-zero.
- **Fixed:** Import completion is now recorded as a `COMMENT ON DATABASE` written only after the
  import succeeds. Keying off `public.placex` alone meant an interrupted import left the table behind
  and was thereafter served as if it had finished. A database imported by an older release is
  validated with `nominatim admin --check-database` and adopted automatically.
- **Fixed:** Passwords containing a space were URL-encoded as `+` in the driver connection string and
  never decoded back, so the role was created correctly and then failed every login.
- **Fixed:** SIGTERM was only handled once Gunicorn was running; stopping the container during an
  import exited 2. The handler is now installed before any long-running work.
- **Fixed:** `.env` is chmod-ed on every write, so a `0644` file left on a volume by an older image is
  corrected instead of kept.
- **Fixed:** `NOMINATIM_*` and `PG*` variables passed to the container reach Nominatim again; the
  first draft's allow-list silently dropped them, unlike the old `sudo -E`.
- **Fixed:** `NOMINATIM_ROLE_OPTIONS` is now applied to an existing managed role, not only at creation.
- **Fixed:** The HEALTHCHECK follows `GUNICORN_BIND` instead of hardcoding `127.0.0.1:8080`.
- **Fixed:** `DEBUG_MODE` was parsed and never used; it now enables redacted verbose logging.
- **Fixed:** `contrib/docker-compose-varnish.yml` pointed `POSTGRES_HOST` at a nonexistent service.
- **Added:** `NOMINATIM_WEBUSER_PASSWORD`, so the read-only API role no longer shares the application
  role's password. Reject `=` as well as `;` in passwords, since both are Nominatim DSN separators.

Serve/import split and immutable root filesystem:

- **Added:** A slim `serve` build target (`docker build --target serve`), published as the `serve`
  and `v<version>-<sha>-serve` tags. It ships without osm2pgsql and postgresql-client, so the
  long-running exposed container is smaller and has less attack surface. It serves an existing
  import and refuses to run an import, failing fast with the remediation in the message; an explicit
  `UPDATE_MODE` on it is a startup error rather than silently stale data.
- **Changed:** Every shipped compose file now runs the container with `read_only: true`. Writes are
  confined to `/nominatim` (volume), `/tmp` and `$HOME` (tmpfs), and `/dev/shm`. The entrypoint
  takes ownership of `$HOME` at startup, because a tmpfs is mounted fresh, and root-owned, on
  every boot.
- **Test:** A `serve_image` integration scenario builds the serve target, asserts osm2pgsql and psql
  are absent, asserts the fail-fast refusal against an empty database, and serves an existing import
  under `--read-only`.

Repository cleanup from an over-engineering audit:

- **Removed:** `example.md`, which duplicated howto.md's configuration table, documented an invalid
  `UPDATE_MODE=none`, and its example ran the upstream `mediagis/nominatim` image.
- **Removed:** The upstream contributors table and `.all-contributorsrc`; every entry pointed at
  `mediagis/nominatim-docker`. The credit is now a link to the upstream list.
- **Changed:** The 16 CI scenarios share one `start-postgres` helper instead of carrying 16 copies
  of the PostgreSQL bootstrap (−291 lines in ci.yml).
- **Removed:** The pre-refactor volume migration path (`FIX_VOLUME_OWNERSHIP` and the ownership
  pre-check). Volumes written by the shell-era image are no longer repaired automatically; chown
  them once by hand if you still have one.
- **Changed:** In-code comments rewritten to be short; the shell-era history they narrated lives in
  docs/REFACTOR.md and git history.
- **Docs:** Fixed stale claims left from before the refactor: `POSTGRES_ADMIN_PASSWORD` was
  documented as defaulting to `NOMINATIM_PASSWORD` (it is required and never derived), the container
  was said to expose PostgreSQL on 5432 (there is no PostgreSQL in this image), the project volume
  was said to hold the import state (it lives in the database now), examples pinned a dead image
  tag, and the never-implemented `API_DB_USER` variable is gone from the docs (`NOMINATIM_WEBUSER`
  is the real one). The manual database setup in EXTERNAL-POSTGIS.md now includes the
  `managed by nominatim-docker` role comments, without which the entrypoint refuses to reconcile
  the roles it did not create.

### v5.3.2 (2026-04-22)

- **Changed:** Merge from upstream (mediagis/nominatim-docker) to sync docs and contributors
- **Changed:** Bump nominatim version to 5.3.2
- **Changed:** Move postgres config in a separate file and mount it in docker compose
- **Fixed:** Corrected `contrib/docker-compose-local.yml` bind mount path (was resolving to contrib/contrib/...), which caused the PostgreSQL container to fail on startup
- **Fixed:** Various markdown files syntax issues
- **Added:** Different postgres configs
- **Added:** New CI helper script `.github/workflows/assert-json-field` to assert specific JSON response fields (dot-path) against regexes with retries
- **Test:** Enhanced CI "API endpoints coverage" scenario to validate status fields, search result fields (name/class/place_rank), addressdetails, polygon GeoJSON, reverse lookup, lookup/details by osmtype+osmid, and Content-Type header

### v5.3.0 (2026-04-04)

- **Changed:** Bump nominatim version to 5.3.0 and varnish image to 8.0.1
- **Changed:** Replace ubuntu:24.04 base image with debian:13.4-slim pinned by digest via ARG BASE_IMAGE so the base can be overridden at build time
- **Changed:** Add ca-certificates to package list for Debian slim compatibility
- **Changed:** Replace update-locale (incompatible in Debian slim) with direct write to /etc/default/locale
- **Changed:** Refactored SCP storage-box credentials in init.sh; use STORAGE_USER / STORAGE_HOST / STORAGE_PASSWORD env vars if you need to change the default ones
- **Fixed:** Fix useradd -p 'plaintext-password' in start.sh (password was stored unencrypted in /etc/shadow); internal nominatim user needs no login pw
- **Changed:** Merge docker-compose-planet.yml with docker-compose.yml structure
- **Changed:** Replace planet postgres command with planet-optimised settings for 64 GB RAM / NVMe SSD
- **Changed:** Switch planet compose to bind mounts (/data/db, /data/nominatim) and replace mediagis image
- **Fixed:** Fix POSTGRES_HOST typo in docker-compose.yml (postgres → nominatim-postgres)
- **Changed:** Default docker-compose.yml now use postgis-18 image
- **Removed:** Removed uvicorn as not required anymore by nominatim
- **Fixed:** Fix gunicorn using the /root folder instead of /home/nominatim

### 2025-10-13

- **Changed:** Documentation update
- **Added:** Added missing in import std; in varnish.vcl
- **Added:** Added missing settings in docker-compose-external-db-varnish.yml
- **Added:** Added a check that verifies if the `REPLICATION_URL` is reachable; if it is set but unreachable the check will set `REPLICATION_URL` to an empty string to avoid crashing Nominatim

### 2025-10-11

- **Changed:** Docker image tags now include Nominatim version: `v<version>-<commit-sha>` (e.g., `v5.1.0-291dcde`)
- **Changed:** Updated documentation to reflect new tag format in README.md and DEPLOYMENT.md
- **Added:** Changelog to track changes included in each release

## Historical Changes

Previous changes were not tracked in a changelog. For historical information, please refer to the git commit history.
