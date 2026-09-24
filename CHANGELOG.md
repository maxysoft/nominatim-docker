# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Releases

### Unreleased: entrypoint rewritten in Go

Upgrading from the shell-based image: [docs/MIGRATION.md](docs/MIGRATION.md). Rationale and parity
matrix: [docs/REFACTOR.md](docs/REFACTOR.md).

- **Breaking:** `NOMINATIM_PASSWORD` is required (the hardcoded default is gone) and
  `POSTGRES_ADMIN_PASSWORD` is required for the initial import, never derived from it. The compose
  files also require `NOMINATIM_WEBUSER_PASSWORD`; set all three in `contrib/.env`.
- **Breaking:** Import completion is recorded in the database (`COMMENT ON DATABASE`, written only
  after the import succeeds) instead of the `import-finished` file, so a populated database is
  never dropped because a volume went missing. A database imported by an older release is
  validated and adopted automatically. Import again with the `reimport` subcommand.
- **Breaking:** An import interrupted part-way is no longer dropped and retried on the next start;
  recover with `docker compose run --rm nominatim-import reimport` or `ALLOW_DROP_EXISTING_DB=true`.
- **Breaking:** The `nominatim` role is created with `CREATEDB` instead of `SUPERUSER`, and missing
  PostGIS/hstore extensions are installed into `template1` (`PROVISION_EXTENSIONS=false` opts out,
  `NOMINATIM_ROLE_OPTIONS=SUPERUSER` restores the old role; a managed role loses `SUPERUSER` once it
  is no longer listed). Roles the container did not create must be tagged
  `managed by nominatim-docker` first; see [docs/MIGRATION.md](docs/MIGRATION.md).
- **Breaking:** `sudo` is gone (use `docker exec -u nominatim ...`); `.env` is regenerated on every
  start, so hand edits are lost; dataset paths must be absolute; `UPDATE_MODE` and the replication
  intervals are validated at startup; a crashed Gunicorn exits non-zero. Volumes written by the
  shell-era image are not repaired automatically: chown them once by hand.
- **Added:** A `serve` build target, published as `serve` and `v<version>-<sha>-serve`: API only,
  without osm2pgsql or postgresql-client, and it refuses to import.
- **Added:** `nominatim-ctl import`, `reimport` and `replicate`. Every `contrib/docker-compose*.yml`
  runs a one-shot `nominatim-import` (full image), the `nominatim` API (serve image, no admin
  credentials) and `nominatim-updater` behind `--profile updates`, all with `read_only: true`.
- **Added:** `POSTGRES_SSLMODE`, `DATA_MIRROR_URL`, `ALLOW_DROP_EXISTING_DB`, `NOMINATIM_WEBUSER`,
  `NOMINATIM_WEBUSER_PASSWORD`, `GUNICORN_BIND`, `GUNICORN_TIMEOUT`, `GUNICORN_GRACEFUL_TIMEOUT`,
  `NOMINATIM_ROLE_OPTIONS`, `PROVISION_EXTENSIONS`, `*_SHA256` checksums and `_FILE` variants for
  the passwords; all listed in [howto.md](howto.md#general-parameters).
- **Added:** A `HEALTHCHECK` on `/status.php`, implemented in the entrypoint (no curl in the image);
  healthy only when Nominatim reports status 0, not merely HTTP 200.
- **Added:** `make check`, `make integration` and `test/integration.sh`, a local stack that imports
  Monaco and asserts the API surface, privilege model, restart and shutdown behaviour.
- **Changed:** `config.sh`, `init.sh` and `start.sh` are replaced by `nominatim-ctl`, a static Go
  binary running as PID 1. Nominatim itself is unchanged. Configuration changes now take effect on
  restart; previously `POSTGRES_HOST`, `NOMINATIM_PASSWORD`, `IMPORT_STYLE` and `REPLICATION_URL`
  were ignored after the first run.
- **Changed:** The API connects as the read-only `www-data` role. A container holding
  `POSTGRES_ADMIN_PASSWORD` reconciles the role passwords, so a rotated password needs no re-import.
- **Changed:** Supplementary datasets come over HTTPS from `nominatim.org` instead of `scp`.
  Downloads resume only a matching partial file, abandon a stalled body, and honour
  `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY`.
- **Changed:** Gunicorn runs in the foreground with `--timeout 60`, `--graceful-timeout 30`,
  `--max-requests 10000` with jitter and `--keep-alive 5`; `GUNICORN_GRACEFUL_TIMEOUT` also bounds
  how long the entrypoint waits before killing it. Continuous replication is supervised: if it
  exits, so does the container. An unreachable `REPLICATION_URL` no longer disables replication;
  it starts once the URL answers. `REPLICATION_URL` takes precedence over `FREEZE` everywhere; a database that is already frozen
  is served without replication (with a warning), and
  `replicate` needs no access to the `postgres` maintenance database.
- **Changed:** `THREADS` and `GUNICORN_WORKERS` default to the container's CPU allowance (cgroup
  quota, at least 2). The compose files size them to PostgreSQL's `max_connections`, and replication
  honours `THREADS`.
- **Changed:** Base image `debian:13.7-slim`, entrypoint built with Go 1.27.1 (refreshed image
  digest), `postgis/postgis:18-3.6-alpine` pins refreshed; all pinned by digest. PyICU comes from
  Debian's `python3-icu`, so the build installs no compiler.
- **Changed:** pgx v5.11.0 (security fixes; passwords containing `@` and other reserved characters
  are now encoded so libpq-style parsing reads them correctly) and golang.org/x/text v0.42.0;
  golang.org/x/crypto is no longer a dependency.
- **Changed:** Python dependencies refreshed, still hash-pinned: gunicorn 26.2.0, urllib3 2.8.0,
  psycopg 3.3.6 and minor bumps.
- **Changed:** The Varnish example runs `varnish:9.0.4` (Debian image); 8.0 is EOL and affected by
  VSV00020. It also sets `IMPORT_WIKIPEDIA: "true"` and `THREADS: 4`; the planet example downloads
  `planet-latest`.
- **Changed:** CI publishes only after the static checks, unit tests and integration scenarios
  pass, runs `govulncheck` on a schedule, and pins every action by commit SHA with a least-privilege
  `permissions:` block. The CI matrix and the docs were simplified.
- **Security:** Removed `sudo`, `sshpass` and `openssh-client`; setuid/setgid bits are stripped, so
  `no-new-privileges:true` is meaningful. Child processes drop root's supplementary groups and no
  longer inherit the database passwords.
- **Security:** Fixed SQL injection through `NOMINATIM_PASSWORD` and `POSTGRES_DB`. Role passwords
  are set with a client-computed SCRAM-SHA-256 verifier (SASLprep applied), so the cleartext never
  reaches the server log. Pre-existing roles without the managed tag are left alone.
- **Security:** `DROP DATABASE` refuses a database holding tables of its own unless
  `ALLOW_DROP_EXISTING_DB=true`, and a database that cannot be inspected never counts as empty.
- **Security:** Secrets are masked in the entrypoint's logs and in child output, including passwords
  embedded in `PBF_URL`, `REPLICATION_URL` or `DATA_MIRROR_URL`. `.env` is written `0600` to a fresh
  file and renamed into place; the serve image's holds only the read-only role's DSN.
- **Security:** Python dependencies are hash-pinned and the build no longer runs an unpinned
  `pip install --upgrade`.
- **Fixed:** Varnish no longer caches 5xx responses and keys the cache on `Accept-Language`.
- **Fixed:** `contrib/docker-compose-varnish.yml` lacked its top-level `networks:` block and pointed
  `POSTGRES_HOST` at a nonexistent service.
- **Fixed:** `max_wal_size` raised to 8GB with a 30 minute `checkpoint_timeout` in both PostgreSQL
  profiles (imports were dominated by WAL-triggered checkpoints), and `shm_size: 1gb` moved from the
  API container to the database service in all four compose files.
- **Removed:** `STORAGE_USER`, `STORAGE_HOST`, `STORAGE_PASSWORD`; `example.md`; the upstream
  contributors table and `.all-contributorsrc` (now a link to the upstream list);
  `docs/VARNISH-PURGE.md`, which described purge features the shipped VCL does not have (purging is
  now covered in `contrib/README-varnish.md`).

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
