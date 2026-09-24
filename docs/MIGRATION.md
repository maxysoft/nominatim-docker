# Migrating from the shell-based image

This guide is for operators running an image built from `master` (the shell entrypoint:
`config.sh`, `init.sh`, `start.sh`) who move to the Go entrypoint (`nominatim-ctl`). Nominatim
itself is the same version (5.3.2), so **your imported database is reused; no re-import is
needed.**

Plan a short outage: on the first start the new image validates the existing database and
records it as imported, which takes a few minutes on a country extract and longer on a planet.

## Before you start

1. **Back up.** Snapshot the PostgreSQL data volume or run `pg_dump` for the Nominatim database.
   The migration does not drop data, but the new roles and markers are written to the cluster.
2. **Write down the passwords in use today.** You need:
   - the PostgreSQL superuser password (`POSTGRES_PASSWORD` of the `nominatim-postgres` service,
     `very_secure_password` in the shipped compose files);
   - `NOMINATIM_PASSWORD`, which on master was the password of **both** the `nominatim` and the
     `www-data` roles. If you never set it, it is the old default `qaIACxO6wMR3`.
3. **Remove `UPDATE_MODE=none`** from your configuration if you set it (see below).

## What changes at a glance

| Area | master | now |
|---|---|---|
| Entrypoint | `/app/start.sh` | `/usr/local/bin/nominatim-ctl`, default command `serve` |
| Compose topology | one `nominatim` container imports, serves and replicates | `nominatim-import` (one-shot), `nominatim` (API, `serve` image), `nominatim-updater` (optional) |
| Secrets | inline in the compose files | `contrib/.env`, required |
| Import detection | `/nominatim/import-finished` file | a marker comment on the database |
| `nominatim` role | `SUPERUSER` | `CREATEDB` |
| API database role | `nominatim` (owner) | `www-data` (read-only), own password |
| Running commands | `docker exec … sudo -u nominatim nominatim …` | `docker exec -u nominatim … nominatim …` |
| Supplementary data | `scp` from a storage box | HTTPS from `https://nominatim.org/data` |

## Environment variables

### Add

| Variable | Default | Notes |
|---|---|---|
| `NOMINATIM_WEBUSER_PASSWORD` | falls back to `NOMINATIM_PASSWORD` with a warning | Password of the read-only `www-data` role. **Required by the compose files.** See step 2 of the compose migration. |
| `NOMINATIM_WEBUSER` | `www-data` | Name of the read-only role. |
| `POSTGRES_SSLMODE` | `prefer` | Any libpq `sslmode`; use `require` or stricter in production. |
| `DATA_MIRROR_URL` | `https://nominatim.org/data` | Replaces the storage-box variables. |
| `PBF_SHA256`, `IMPORT_WIKIPEDIA_SHA256`, … (`_SHA256` on every `IMPORT_*` switch) | unset | Optional checksum of each download. |
| `NOMINATIM_ROLE_OPTIONS` | `CREATEDB` | Set `SUPERUSER` only if your provider cannot pre-install extensions. |
| `PROVISION_EXTENSIONS` | `true` | Installs missing PostGIS/hstore into `template1` before an import. Set `false` on a shared cluster. |
| `ALLOW_DROP_EXISTING_DB` | `false` | Lets an import drop a database that holds tables. Prefer the `reimport` command. |
| `GUNICORN_BIND`, `GUNICORN_TIMEOUT`, `GUNICORN_GRACEFUL_TIMEOUT` | `0.0.0.0:8080`, `60`, `30` | Raising the drain deadline also needs `stop_grace_period` raised (see compose changes). |
| `NOMINATIM_PASSWORD_FILE`, `POSTGRES_ADMIN_PASSWORD_FILE`, `NOMINATIM_WEBUSER_PASSWORD_FILE` | unset | Read the password from a file (Docker secrets). |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | unset | Now honoured by every download and by replication. |

### Remove

| Variable | Why |
|---|---|
| `STORAGE_USER`, `STORAGE_HOST`, `STORAGE_PASSWORD` | Datasets are no longer fetched over `scp`. Use `DATA_MIRROR_URL` if you mirror them. |
| `UPDATE_MODE=none` | No longer a valid value and **fails at startup**. Leave `UPDATE_MODE` unset for no updates. |
| `shm_size` / `--shm-size` on the Nominatim container | It no longer runs PostgreSQL work that needs it; set it on the PostgreSQL container instead. |

### Changed meaning

- **`NOMINATIM_PASSWORD` is required.** The built-in default `qaIACxO6wMR3` is gone. Must not
  contain `;` or `=`.
- **`POSTGRES_ADMIN_PASSWORD` is required for an import** and no longer defaults to
  `NOMINATIM_PASSWORD`. A container that has it also keeps the role passwords in sync with the
  environment on every start.
- **Configuration applies on every start.** master only read `POSTGRES_HOST`,
  `NOMINATIM_PASSWORD`, `IMPORT_STYLE` and `REPLICATION_URL` on the first run; now the project
  `.env` is regenerated from the environment each time, so hand edits to it are lost.
- **`THREADS` and `GUNICORN_WORKERS`** default to the container's CPU allowance (the cgroup quota)
  instead of `nproc`. The compose files now set both explicitly.
- **`FREEZE`** is ignored when `REPLICATION_URL` is set. A database frozen earlier stays frozen
  and is served without updates.
- **Stricter validation at startup:** `UPDATE_MODE` must be `continuous`, `once` or `catch-up`;
  `REPLICATION_UPDATE_INTERVAL`/`REPLICATION_RECHECK_INTERVAL` must be integers; dataset paths
  (`IMPORT_WIKIPEDIA=/path/...`) must be absolute.

Unchanged: `PBF_URL`, `PBF_PATH`, `REPLICATION_URL`, `IMPORT_STYLE`, `REVERSE_ONLY`,
`IMPORT_WIKIPEDIA`, `IMPORT_SECONDARY_WIKIPEDIA`, `IMPORT_US_POSTCODES`, `IMPORT_GB_POSTCODES`,
`IMPORT_TIGER_ADDRESSES`, `WARMUP_ON_STARTUP`, `DEBUG_MODE`, `POSTGRES_HOST`, `POSTGRES_PORT`,
`POSTGRES_DB`, `PROJECT_DIR`, `USER_AGENT`.

## Docker Compose changes

The four files in `contrib/` keep their names, the `nominatim-postgres` service and the volume
names, so an existing project picks up its data. What changed:

- **Three Nominatim services instead of one.**
  - `nominatim-import` (full image, `command: import`) provisions, imports or adopts, then exits.
    It is the only container with `POSTGRES_ADMIN_PASSWORD`.
  - `nominatim` (the `serve` image) runs the API on port 8080 with no import tooling and no admin
    credentials. It starts only after the import has completed, and uses its own small volume,
    `nominatim-serve-data`.
  - `nominatim-updater` (full image, `command: replicate`) applies replication diffs. It runs only
    with `--profile updates`, in continuous mode unless `UPDATE_MODE` says otherwise. This
    replaces `UPDATE_MODE=continuous` on the old single container.
  - The import and the updater share the existing `nominatim-data` volume (the `/data/nominatim`
    bind in the planet file), so project options such as `IMPORT_STYLE` go in the shared
    `x-replication` block, not in one service.
- **Secrets come from `contrib/.env`.** Every file refuses to start without
  `POSTGRES_ADMIN_PASSWORD`, `NOMINATIM_PASSWORD` and `NOMINATIM_WEBUSER_PASSWORD`; the
  PostgreSQL service takes its `POSTGRES_PASSWORD` from `POSTGRES_ADMIN_PASSWORD`.
- **Hardening:** every Nominatim container runs with `read_only: true`, `tmpfs` for `/tmp` and
  `$HOME`, `no-new-privileges`, `cap_drop: ALL` plus the six capabilities the entrypoint needs,
  and `init: true`. If you mount extra paths, mount them writable only where Nominatim writes.
- **`stop_grace_period: 40s`** on every Nominatim container, so Gunicorn can drain and a
  replication step can finish. Keep it above `GUNICORN_GRACEFUL_TIMEOUT` + 5 s.
- **`shm_size: 1gb` moved** from the Nominatim container to `nominatim-postgres`, where parallel
  query workers use it.
- **Connection budget:** the API sets `GUNICORN_WORKERS` and `NOMINATIM_API_POOL_SIZE: 5`, and the
  shared block sets `THREADS`, so workers × 5 + `THREADS` stays below each file's
  `max_connections`. Scale them together.
- **Images:** the import and updater use `ghcr.io/maxysoft/nominatim-docker:latest`, the API uses
  `…:serve`. Published tags are `latest`, `serve`, `v<version>-<sha>` and `v<version>-<sha>-serve`.
- **`docker-compose-local.yml`** builds two local images, `nominatim:local` and
  `nominatim:local-serve`, and no longer sets `container_name`; the API port is
  `${NOMINATIM_PORT:-8080}`.

## PostgreSQL changes

- **Same server.** The PostgreSQL 18 / PostGIS 3.6 image is unchanged apart from a newer digest of
  the same tag, so no dump and restore is needed.
- **Tuning:** `16g-postgresql.conf` and `postgresql.conf` raise `max_wal_size` from 2–3 GB to 8 GB
  and `checkpoint_timeout` from 10–20 min to 30 min, which stops the checkpoint storm during
  imports. Allow for up to 8 GB of WAL on the database disk. The planet and local profiles are
  unchanged.
- **Roles.** The container only manages roles tagged with the comment
  `managed by nominatim-docker`; it refuses to change the password of any other role. The roles
  master created are untagged, so you tag them once (step 2 below). Once tagged:
  - the `nominatim` role loses `SUPERUSER` and keeps `CREATEDB`, unless
    `NOMINATIM_ROLE_OPTIONS=SUPERUSER`;
  - both role passwords follow `NOMINATIM_PASSWORD` and `NOMINATIM_WEBUSER_PASSWORD` on every start
    of a container that has `POSTGRES_ADMIN_PASSWORD`.
- **Import marker.** A completed import is recorded as `COMMENT ON DATABASE nominatim IS
  'nominatim-docker: import complete'`. On the first start, a database imported by master (tables
  present, no marker) is checked with `nominatim admin --check-database` and adopted. The
  `import-finished` file is no longer read.
- **Drop guard.** An import never drops a database that holds tables unless you run `reimport` or
  set `ALLOW_DROP_EXISTING_DB=true`. An import interrupted part-way is no longer retried
  automatically; recover with `reimport`.
- **`template1`** receives PostGIS, `postgis_raster` and hstore before a **new** import unless
  `PROVISION_EXTENSIONS=false`. Adopting your existing database does not touch it.

## Step by step: Docker Compose

The examples use `contrib/docker-compose.yml`; substitute the file you run. Run them from the
repository root.

**0. Stop the old stack, keeping its volumes**, while the old `contrib/` files are still checked
out:

```sh
docker compose -f contrib/docker-compose.yml down        # no -v: that would delete the data
```

The networks differ between the old and new files, so an old container left running blocks the
new stack. If you already updated the checkout, remove the old containers by name instead:
`docker rm -f nominatim` (`nominatim-local` for the local file, plus `nominatim-varnish` for the
Varnish file).

**1. Update the checkout and create `contrib/.env`.**

```sh
git pull
cp contrib/.env.example contrib/.env
```

Set `POSTGRES_ADMIN_PASSWORD` to the **current** superuser password (the PostgreSQL image applies
`POSTGRES_PASSWORD` only when it creates the data directory, so a new value here does not change
it). Set `NOMINATIM_PASSWORD` to the value you used on master. Choose a new
`NOMINATIM_WEBUSER_PASSWORD`.

Carry over your own settings (`PBF_URL`, `REPLICATION_URL`, `IMPORT_*`, `THREADS`, …) into the
compose file you use. Drop `STORAGE_*`, `UPDATE_MODE=none` and `shm_size` on the Nominatim
service.

**2. Tag the existing roles** so the container may manage them, then check that it took:

```sh
docker compose -f contrib/docker-compose.yml up -d --wait nominatim-postgres
docker compose -f contrib/docker-compose.yml exec nominatim-postgres psql -U postgres \
  -c "COMMENT ON ROLE nominatim IS 'managed by nominatim-docker'" \
  -c "COMMENT ON ROLE \"www-data\" IS 'managed by nominatim-docker'"
docker compose -f contrib/docker-compose.yml exec nominatim-postgres psql -U postgres -tAc \
  "SELECT rolname, shobj_description(oid, 'pg_authid') FROM pg_roles WHERE rolname IN ('nominatim', 'www-data')"
```

Both rows must end in `managed by nominatim-docker`. Without the tag, the import container logs
`role passwords not reconciled`, the new `NOMINATIM_WEBUSER_PASSWORD` is never applied, and the API
cannot log in. If you do not want to tag the roles, set `NOMINATIM_WEBUSER_PASSWORD` to your old
`NOMINATIM_PASSWORD` instead.

**3. Fix the ownership of the data volume.** The new image runs as uid/gid 1000 and does not
re-own files written by the old one. Compose prefixes volume names with the project name, which
is `contrib` for the shipped files (check with `docker volume ls`):

```sh
docker run --rm -u 0 --entrypoint chown \
  -v contrib_nominatim-data:/nominatim \
  ghcr.io/maxysoft/nominatim-docker:latest -R 1000:1000 /nominatim
```

For the planet file, run `chown -R 1000:1000 /data/nominatim` on the host.

**4. Start the stack.**

```sh
docker compose -f contrib/docker-compose.yml up -d --remove-orphans
docker compose -f contrib/docker-compose.yml logs -f nominatim-import
```

The import container should log `holds tables but no completion marker; validating`, then
`validation passed; adopting the existing import`, and exit 0, without a `role passwords not
reconciled` warning. The API starts after it and turns healthy once `/status.php` reports
status 0.

**5. Turn updates back on** if the old container ran with `UPDATE_MODE=continuous`:

```sh
docker compose -f contrib/docker-compose.yml --profile updates up -d
```

## Step by step: single container (`docker run`)

The full image still imports and serves in one container, so an existing `docker run` keeps
working with these changes:

- pass `-e NOMINATIM_PASSWORD=…` explicitly (no default any more);
- add `-e POSTGRES_ADMIN_PASSWORD=…` if the container should reconcile roles or import;
  it is not needed to serve an existing import;
- remove `--shm-size`, the `STORAGE_*` variables and `UPDATE_MODE=none`;
- tag the roles (step 2 above, with `psql` against your server) and chown the volume (step 3);
- replace `docker exec … sudo -u nominatim nominatim …` with `docker exec -u nominatim … nominatim …`.

Without `NOMINATIM_WEBUSER_PASSWORD`, the API role keeps sharing `NOMINATIM_PASSWORD`, as on
master, and the container logs a warning.

## Varnish

`contrib/docker-compose-varnish.yml` moves from `varnish:8.0.1-alpine` to `varnish:9.0.4`
(Debian based; 8.0 is end of life and affected by VSV00020).

- The service no longer passes `default_ttl`/`default_grace` on the command line, drops
  `cap_add: IPC_LOCK` and lowers the memlock limit to 100 MB. It now restarts with the API, so it
  re-resolves the backend when the API container is recreated.
- `contrib/varnish.vcl` no longer caches 5xx responses and adds `Accept-Language` to the cache key.
  A **custom VCL** must be checked against the Varnish 9 changes (`beresp.storage_hint` removed,
  `req.ttl` renamed `req.max_age`); compile it with
  `docker compose -f contrib/docker-compose-varnish.yml run --rm --no-deps --entrypoint varnishd nominatim-varnish -C -f /etc/varnish/default.vcl`.
- The image has no `curl` or `wget`; see [contrib/README-varnish.md](../contrib/README-varnish.md)
  for troubleshooting and purging.

## Other behaviour changes

- `sudo` is not in the image; `/app/*.sh` are gone. Subcommands: `nominatim-ctl serve`, `import`,
  `reimport`, `replicate`, `config` and `healthcheck`.
- Replication and Gunicorn log to the container output (`docker logs`), not
  `/var/log/replication.log`.
- The image has a `HEALTHCHECK` on `/status.php`: healthy only when it reports status 0, so an
  API that cannot reach its database is unhealthy. The start period is 48 h for long imports.
- A crashed Gunicorn, or a continuous replication that stops, now exits the container non-zero,
  so the restart policy brings it back. A `docker stop` still exits 0.
- To import a newer extract into the same database:
  `docker compose -f contrib/docker-compose.yml run --rm nominatim-import reimport`.

## Verify

```sh
curl -s 'http://localhost:8080/status.php?format=json'        # "status":0
docker compose -f contrib/docker-compose.yml exec nominatim-postgres psql -U postgres -tAc \
  "SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = 'nominatim'"
                                                                 # nominatim-docker: import complete
docker compose -f contrib/docker-compose.yml exec nominatim-postgres psql -U postgres -tAc \
  "SELECT rolname, rolsuper, shobj_description(oid, 'pg_authid') FROM pg_roles WHERE rolname IN ('nominatim', 'www-data')"
```

## Troubleshooting

| Message | Cause and fix |
|---|---|
| `NOMINATIM_PASSWORD must be set` | Set it in `contrib/.env` or with `-e`. |
| `POSTGRES_ADMIN_PASSWORD must be set to provision the database` | The container has to import (no completed import found) and has no admin password. |
| `UPDATE_MODE must be one of continuous, once, catch-up` | Remove `UPDATE_MODE=none`. |
| `role "…" already exists and is not managed by this image` | Tag the role (step 2). |
| `PostgreSQL rejected the credentials` | `POSTGRES_ADMIN_PASSWORD` is not the cluster's current superuser password, or `NOMINATIM_PASSWORD` differs from the role's password on an untagged role. |
| API: `password authentication failed for user "www-data"` | The roles are untagged and `NOMINATIM_WEBUSER_PASSWORD` is new. Tag them, or set it to the old `NOMINATIM_PASSWORD`. |
| `contains an incomplete or invalid Nominatim schema` | `--check-database` failed on the existing database. Run `docker exec -u nominatim <container> nominatim admin --check-database --project-dir /nominatim` to see why; after fixing, start again, or re-import with `reimport`. |
| `permission denied` under `/nominatim` | Chown the volume (step 3). |
