# Internals

How `nominatim-ctl` and the image work, and why: the constraints, failure modes and design decisions behind the code. For maintainers. Operators start at the [README](../README.md); the shell-to-Go rewrite is covered in [REFACTOR.md](REFACTOR.md).

- [Entrypoint and commands](#entrypoint-and-commands)
- [Configuration](#configuration)
- [Import lifecycle](#import-lifecycle)
- [Database provisioning and safety](#database-provisioning-and-safety)
- [Downloads](#downloads)
- [Process supervision](#process-supervision)
- [Secret masking](#secret-masking)
- [Image build](#image-build)
- [Make targets](#make-targets)
- [CI](#ci)
- [Integration tests](#integration-tests)
- [Unit tests](#unit-tests)

## Entrypoint and commands

### Subcommands

`cmd/nominatim-ctl/main.go` is the container entrypoint. It renders the project configuration, provisions the external PostgreSQL database, runs the import when one is needed, and supervises the API server.

- No argument or `serve`: `ctl.Serve`.
- `import`: `ctl.EnsureImported`, the same "import, adopt or skip" decision `serve` makes. A one-shot import service can therefore run on every `docker compose up` without dropping anything.
- `reimport`: sets `AllowDropExistingDB` and calls `ctl.RunImport` directly. It is the one-shot "drop and import again" command, used as `docker compose run --rm nominatim-import reimport`.
- `replicate`: `ctl.Replicate`.
- `config`: prints the rendered `.env` to stdout. The output contains the web role's DSN, password included.
- `healthcheck [host:port]`: see below.
- Anything else prints the usage line to stderr and exits 2.

### Healthcheck bind address

`healthcheck` runs before `ctl.Load`, so it needs no configuration or secrets. The address it probes is, in order: the second argument; `GUNICORN_BIND` rewritten by `loopback`; `127.0.0.1:8080`. `loopback` turns a wildcard bind (`:8080`, `0.0.0.0:8080`) into `127.0.0.1:<port>`, which is reachable from inside the container, so the healthcheck follows a changed `GUNICORN_BIND` without its own setting. Any other address is used unchanged.

### Signals and exit semantics

The SIGTERM/SIGINT handler (`signal.NotifyContext`) is installed right after the configuration loads and before any long-running work. A stop during a multi-day import therefore cancels the context and exits through the normal path, instead of the Go runtime's default handling. Without the handler the runtime would re-raise the signal with the default action; as PID 1 the kernel ignores that, so the runtime falls back to exiting with status 2.

- Error from any command: `fatal: <error>` on stderr (redacted), exit 1.
- Stop of `serve` or `replicate`: logs `shutdown complete`, exit 0.
- Stop of `import` or `reimport` that left the import unfinished: exit 1 with `import interrupted before completion`. A partial database must not satisfy a compose `depends_on` condition of `service_completed_successfully`.

All three role passwords and the URL credentials (see "Secret masking") are registered for masking right after `ctl.Load` returns. Messages logged inside `Load` are therefore not masked and must not contain secrets.

## Configuration

### Loading and validation

`Load` in `internal/ctl/config.go` resolves the whole configuration once at startup. Its only I/O is reading `*_FILE` secret files and stat-ing the flatnode directory and dataset paths. Only `Load` validates: code that reads a variable with `os.Getenv` elsewhere must validate it itself.

Validation is split in two:

- `Validate` (called by `Load`) rejects configuration that cannot produce a working container in any mode.
- `ValidateForImport` adds the checks that only matter when a fresh import runs: exactly one of `PBF_URL` / `PBF_PATH`, a readable `PBF_PATH`, and `POSTGRES_ADMIN_PASSWORD`. A restart against an already-imported database therefore does not need the original PBF settings.

`Load` also checks the format of `PBF_SHA256` and every `IMPORT_*_SHA256` (64 hex digits). The import reads those variables itself, and `verifyChecksum` would only reject a malformed value after the whole download.

`REPLICATION_UPDATE_INTERVAL` and `REPLICATION_RECHECK_INTERVAL` (seconds) must be non-negative integers, and setting either without `REPLICATION_URL` is an error, because they mean nothing without it.

`THREADS` and `GUNICORN_WORKERS` default to `runtime.GOMAXPROCS(0)`. Since Go 1.25 that follows the cgroup CPU quota, rounded up, with a minimum of 2.

A directory mounted at `$PROJECT_DIR/flatnode` opts the import into flatnode storage (`$PROJECT_DIR/flatnode/flatnode.file`). Without the directory, no flatnode file is configured.

The bootstrap superuser used with `POSTGRES_ADMIN_PASSWORD` is always named `postgres` (`adminUser`).

### Datasets

Each `IMPORT_*` variable in `Datasets` is dual-typed: `true` downloads the file from `DATA_MIRROR_URL`, an absolute path links an existing file, and empty or `false` disables the dataset. `DatasetValues` keeps the raw value per variable name. `Remote` is the file name on the mirror and `Local` the name Nominatim expects in the project directory. They differ for the secondary importance dump (`wikimedia-secondary-importance.sql.gz` becomes `secondary_importance.sql.gz`) and for Tiger (`tiger2024-…` becomes `tiger-nominatim-preprocessed.csv.tar.gz`).

### Secrets from files

`envSecret` reads `NAME`, or the contents of the file named by `NAME_FILE` when that is set, with trailing CR/LF removed. The file form keeps secrets out of `docker inspect` and out of the environment that child processes inherit.

### Connection strings

`DSN` builds Nominatim's `pgsql:key=value;…` form. Nominatim splits it on `;` and each field on `=`, and there is no escaping, so `Validate` rejects either character in `NOMINATIM_PASSWORD` and `NOMINATIM_WEBUSER_PASSWORD`.

`LibpqURL` builds the URL the pgx connections use (database `postgres` when none is given). It goes through `url.URL` on purpose: that escapes `@`, `:` and `/` in the userinfo, and pgx, like libpq, ends the userinfo at the first `@`. `net.JoinHostPort` brackets an IPv6 host literal. The password is passed through `saslprep` first, for the reason in REFACTOR.md "A note on password normalisation".

### The project `.env`

`RenderEnvFile` regenerates the whole file on every start. It carries only the read-only web role's DSN. Child processes started by `nominatim-ctl` get the owning role's DSN through their environment (`BaseEnv`), and Nominatim prefers the environment over `.env`.

`WriteEnvFile` never writes `.env` in place. `PROJECT_DIR` is writable by the workload user, so `.env` may be a planted symlink, and the writer runs as root. It writes a temporary file in the same directory and renames it over `.env`, which replaces a symlink instead of following it. The temporary file comes from `os.CreateTemp`, which creates it with mode 0600; that is the final mode of `.env` (it holds a cleartext password), so replacing `CreateTemp` with a call that takes a default mode would loosen it. As root the file is chowned to the workload user before the rename.

A crash before the rename leaves a `.env.tmp-*` file holding the DSN behind. Each write removes such files once they are older than one minute; a younger one may belong to a process that is writing right now. A rename that fails with `EBUSY` means `.env` is bind-mounted, which is refused with an explanatory error.

## Import lifecycle

### Entry points

`nominatim-ctl import` and `serve` call `EnsureImported` in `internal/ctl/serve.go`, which decides whether to import. `nominatim-ctl reimport` sets `ALLOW_DROP_EXISTING_DB` and calls `RunImport` in `internal/ctl/import.go`, which imports unconditionally. Both paths run `checkImportable` before any download or provisioning: it validates the import configuration and refuses in the serve-only image, which lacks osm2pgsql.

### EnsureImported decision flow

1. Wait until the server is reachable. With admin credentials, probe the `postgres` database as the admin role; without them, probe `POSTGRES_DB` as `nominatim`. The decision waits for reachability so that a booting database is never mistaken for an empty one on a routine restart.
2. With admin credentials, take the import lock before reading the marker, so a container that waited sees the other container's finished import. Without admin credentials nothing can be dropped or imported, and adoption is idempotent, so no lock is taken.
3. Read the marker over the probe connection. The marker read, the table probe and the validator's connection test share a 30-second bound, so a server that accepts TCP but stalls ends in an error, never a hang. An error here never leads to a drop.
4. Marker present: reconcile the roles and chown the project files. A role reconciliation failure is only a warning, because adopted roles lack the marker.
5. No marker: probe for tables, as the admin role when available. A probe error stops the start unless `ALLOW_DROP_EXISTING_DB=true`; with the flag set, the database is treated as populated and validated.
6. Tables but no marker: an import that died part-way, a restored or adopted import never stamped, or not a Nominatim database at all. Nominatim's validator decides which, and only a failing database may be dropped.
7. No database or no tables: import.

Before validating, `EnsureImported` reconciles the roles. The validator connects as `nominatim`, which a restore onto a fresh server lacks and a rotated password locks out. It then tests that connection. A failed connection says nothing about the schema, so it stops the start with nothing dropped. `nominatim admin --check-database` then runs. On failure, the start stops unless `ALLOW_DROP_EXISTING_DB=true` and the context is still live; then the database is dropped and imported again.

### Adoption

A database that passes validation is adopted. `EnsureImported` runs the steps a fresh import runs after its own check: `configureReplicationOrFreeze` and `finishImport`. The adopted database was not created by this image, so the `nominatim` role may lack ownership. Both steps therefore only warn on failure, because the import is valid either way. When stamping fails, the log prints the exact `COMMENT ON DATABASE` statement for the owner to run. An unstamped database is served and validated again on every start.

On every start, `Serve` runs `nominatim refresh --functions`. An adopted database owned by another role cannot have its functions replaced, and they already exist in a validated database, so there the failure is a warning. On a database the `nominatim` role owns, the failure is real and stops the start.

### Import steps

`runImport` in `internal/ctl/import.go` runs, in order:

1. Fetch the supplementary datasets and the OSM extract (`PBF_SHA256` verifies a downloaded one).
2. `provisionDatabase`: roles, drop guard, extensions, all over the admin connection. The application role is not a superuser.
3. Chown the project files, then `nominatim import`, and Tiger `add-data` when enabled.
4. `nominatim index`. The import can leave parent places flagged for indexing, and `--check-database` would then report unindexed entries.
5. `nominatim admin --check-database`.
6. Replication `--init` or freeze. `REPLICATION_URL` wins over `FREEZE`. When the URL is unreachable, `--init` is skipped with a warning; serve and the updater run `--init` again on every start, so a passing outage does not turn replication off for good.
7. Warm the caches. `warmCaches` sets `NOMINATIM_QUERY_TIMEOUT` to 600 seconds and `NOMINATIM_REQUEST_TIMEOUT` to 3600 seconds for the warm-up pass.
8. `finishImport`: `ANALYZE`, write the marker, then remove the downloads. The marker is written only after full success, so an interrupted run leaves tables without a marker, and the next start validates them.

### Dataset files

Nominatim's import reads the supplementary dataset files from the project directory by fixed name and imports whatever it finds there. A disabled dataset must therefore not leave a file of this image's there.

A file at a dataset's path belongs to this image when it is a symlink, or a download with its `.source` state file next to it (`statePath`). Any other file belongs to the operator. `isOperatorFile` and `removeLeftover` in `internal/ctl/import.go` apply this rule, and nothing ever removes an operator file.

`fetchDatasets` handles each value of the dataset variable:

- `true`: download the file. An operator file at the path stops the import: the download would truncate it, and a checksum mismatch would delete it.
- An absolute path naming the file at the dataset's own path: use it in place, and delete the state file. Without the state file, `cleanupDownloads` treats the file as the operator's and keeps it.
- Any other absolute path: remove this image's leftovers and symlink the path in. An operator file at the dataset's path stops the import.
- Unset or `false`: remove this image's leftovers. An operator file stays, with a warning that Nominatim imports it anyway. Tiger is the exception: only the explicit `add-data` step in `runImport` reads it.

`cleanupDownloads` removes exactly the files the import created, never by glob: dataset leftovers by the ownership rule, and the OSM extract with its state file when it was downloaded.

`chownProjectFiles` runs only as root. It chowns, without following symlinks, the project directory, the env file, the dataset paths, a downloaded extract and the flatnode file with its directory. It never recurses, so operator data mounted under the project directory keeps its ownership.

## Database provisioning and safety

### Connection waits

`waitForDatabase` in `internal/ctl/db.go` retries a connection with a 10-second timeout per attempt; callers use 150 attempts 2 seconds apart. It logs the first attempt and every tenth. Two errors end the wait at once, because retrying cannot change them:

- SQLSTATE class 28 (invalid authorization): rejected credentials.
- SQLSTATE 3D000 (`invalid_catalog_name`): the server answered and the database does not exist. The caller decides what absence means.

Every other error retries, including 57P03 (server starting up) and refused connections. `isMissingDatabase` is the only connect error that proves a database holds nothing; every other error leaves its contents unknown, and `databaseHasTables` returns it rather than reporting the database empty.

### Roles

`ensureRole` in `internal/ctl/db.go` creates a missing role and its `managed by nominatim-docker` marker comment in one transaction. A role created without its marker would be refused as foreign on every later start.

For an existing managed role, `ensureRole` resets the password on every start, so rotation takes effect, and re-applies the role options. Without the re-apply, `NOMINATIM_ROLE_OPTIONS` would only take effect on the run that created the role.

`reconcileRoleOptions` adds `NOSUPERUSER` only when the role is currently a superuser and the options no longer grant `SUPERUSER`. This makes removing `SUPERUSER` from `NOMINATIM_ROLE_OPTIONS` take effect. It is issued only in that case, because `NOSUPERUSER` itself needs superuser to issue. `roleIsSuperuser` in `internal/ctl/import.go` matches whole words case-insensitively; a substring test would also match `NOSUPERUSER`.

`reconcileRoles` reconciles the `nominatim` role with `NOMINATIM_ROLE_OPTIONS` (default `CREATEDB`, which is all the import needs once PostGIS is pre-installed) and the web role with no options. When `NOMINATIM_WEBUSER` is `nominatim`, the second call is skipped: on the same role, the option-less call would reconcile the options away and revoke `SUPERUSER`.

### Passwords and SQL quoting

`passwordSecret` in `internal/ctl/db.go` builds a SCRAM-SHA-256 verifier with a 16-byte random salt and `scramIterations` = 4096, PostgreSQL's own default. `scramVerifier` writes the RFC 5802 format PostgreSQL stores in `pg_authid.rolpassword`: `SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>`. `saslprep` falls back to the raw password when SASLprep preparation fails, as PostgreSQL's `pg_saslprep` does, so the verifier and the server agree for any input.

`CREATE ROLE … PASSWORD`, `ALTER ROLE … PASSWORD` and `COMMENT ON` cannot take bind parameters, so `QuoteLiteral` renders their values as literals; an unescaped quote would let the rest of the value run as SQL. `QuoteLiteral` doubles single quotes. When the value contains a backslash, it switches to an `E''` literal and doubles each backslash. `mustNotBeEmpty` rejects an empty role name or `POSTGRES_DB` before either reaches a statement such as `DROP DATABASE`.

### Import marker and database state

The marker is a `COMMENT ON DATABASE` written only after the import fully succeeded. The existence of `public.placex` is no signal: Nominatim creates it early in the import.

`readImportMarker` reads the marker from `pg_database` through `shobj_description`, so a connection to the maintenance database is enough. This works where `pg_hba.conf` keeps the admin role out of `POSTGRES_DB`. Rows with `datconnlimit = -2` are excluded: PostgreSQL 16 and later mark a database invalid this way when `DROP DATABASE` is interrupted, and such a database can keep its comment.

`markImportComplete` needs ownership of the database. The `nominatim` role owns a fresh import; for an adopted one, `finishImport` connects as the admin role when admin credentials are set.

`databaseState` reports two facts. A database is frozen when `public.place` is gone, which is Nominatim's own test after `nominatim freeze`. It is owned by the application when its owner is `nominatim`; an adopted database may belong to another role.

### Import lock

The advisory lock is taken on a connection to the maintenance database, because advisory locks are per database and the target database may be dropped. `test/integration.sh` takes the same lock by the value of `importLockClass`, so the two must change together. Both `RunImport` (`reimport`) and `EnsureImported` take the lock, and both wait while another container holds it.

### Drop guard

`dropDatabase` in `internal/ctl/db.go` adds `WITH (FORCE)` on PostgreSQL 13 and later (`server_version_num` ≥ 130000), to terminate sessions a previous container left behind. The version is cast to `int` on the server, because pgx does not scan text into an int. If reading the version fails, the drop runs without `FORCE`.

When a database cannot be inspected, the import refuses to drop it unless `ALLOW_DROP_EXISTING_DB=true`. With the flag set, it is treated as populated, and the drop proceeds. This is the only way out for a database left invalid by an interrupted `DROP DATABASE` on PostgreSQL 16 and later, which cannot be connected to. Callers bound the table probe: `provisionDatabase` with 15 seconds, so a server that accepts TCP but stalls the handshake fails with a diagnostic instead of hanging.

### Extensions

`provisionExtensions` installs `hstore`, `postgis` and `postgis_raster` into `template1`. The list must match what `setup_database_skeleton` in Nominatim's `nominatim_db` creates; check it when upgrading Nominatim. PostGIS is an untrusted extension, so `CREATE EXTENSION` needs a superuser.

`provisionDatabase` in `internal/ctl/import.go` skips this step when `NOMINATIM_ROLE_OPTIONS` grants `SUPERUSER`, because the import then creates the extensions itself. It closes the `template1` connection immediately instead of deferring the close: the import's `CREATE DATABASE … TEMPLATE template1` fails while any session is attached to `template1`.

## Downloads

### Transport

`NewDownloader` in `internal/ctl/download.go` is tuned for multi-GB files on slow links. There is no overall deadline, but every phase that can hang is bounded: the default transport's dial timeout, a 30 s TLS handshake (the default 10 s is too short here), a 60 s wait for response headers, and a 60 s idle timeout on the body (`IdleTimeout`). The transport is a clone of `http.DefaultTransport`, so `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` apply. Every request sets `User-Agent`, because nominatim.org rejects requests without one.

TLS verification must never be relaxed for a mirror: one downloaded artifact, the secondary-importance SQL dump, is executed against the database ([REFACTOR.md](REFACTOR.md), C3).

### Retries

`Fetch` makes up to `fetchAttempts` (5) attempts per file, waiting `Backoff` (2 s) before the second and doubling each time. A planet PBF takes hours to download, and one transient reset must not discard it; each retry resumes where the previous attempt stopped.

Network errors, 408, 429, 5xx, a `Content-Range` mismatch, an unusable 416 and an idle timeout are transient. Other status codes and local file-system errors are wrapped in `errPermanent` and end the loop at once, as does a checksum mismatch or a cancelled context.

The idle timeout works through `idleReader`: every read that returns data resets a timer, and the timer cancels the attempt's context. The error then names the idle period, which distinguishes a stall from a network error.

### State file and resume

Next to each download, `<dest>.source` holds a JSON `fetchState`: the URL, a validator, and whether the file is complete. The validator is the strong `ETag`, else `Last-Modified`; a weak ETag (`W/…`) is not allowed in `If-Range`. An unreadable state file means "start over".

`fetchOnce` resumes only its own partial download of the same URL and version: the state must name the URL and carry a validator. It sends `Range` with `If-Range`, so if upstream republished the file meanwhile, the server answers 200 with the whole new file instead of splicing new bytes onto the old partial. Without a validator there is no safe `If-Range`, so the download starts from zero.

- 200: the file is truncated and rewritten. The new state is written only after the `O_TRUNC` open; written earlier, a crash would leave the new version's state over the old bytes and the next run would resume onto them.
- 206: accepted only when the `Content-Range` start equals the local file size. `parseContentRangeStart` may parse loosely because of that equality check. On a mismatch the partial file is removed and the attempt retried from zero.
- 416 with a total equal to the local size: the download had finished but its completion was never recorded (the process died right after the last byte). This is trusted only when `If-Range` was sent (offset > 0), because that confirms the version. Otherwise the local file is longer than the remote one or cannot be verified, and it is removed.

A state marked complete is reused without a request if the checksum still matches. If the operator updated `*_SHA256` after upstream republished, `verifyChecksum` removes the old file and the download starts again.

### Checksums

When a `*_SHA256` is set, `verifyChecksum` hashes the completed file and compares case-insensitively. On a mismatch it removes the file and its state file, so a later run starts clean instead of resuming onto bad bytes, and the error is permanent.

### Symlink safety

The project directory is writable by the workload user, while `nominatim-ctl` may run as root, so no path there is written through a symlink:

- A symlink at the destination, for example left by an earlier `IMPORT_X=/path` run, is removed before downloading. Writing through it would overwrite the operator's own file.
- The download opens the file with `O_NOFOLLOW`.
- `writeFileNoFollow` (used for the state file) removes the path and recreates it with `O_EXCL|O_NOFOLLOW`.
- A file that must be discarded is removed, never truncated: `os.Truncate` would follow a symlink swapped in meanwhile.

### Replication URL probe

`Reachable` sends `HEAD` requests (10 s each, `attempts` tries, `delay` apart) and treats any status below 400 as reachable. The caller uses it to decide whether replication can be configured at all. Each failed attempt is logged with its status or error, so an unreachable server can be told apart from a typo in the URL.

## Process supervision

### Running children as the nominatim user

`Runner` in `internal/ctl/run.go` starts every Nominatim, Gunicorn and replication process. `nominatimUser` in `internal/ctl/run.go` and `nominatimHome` in `internal/ctl/serve.go` must match the account the Dockerfile creates. That account's UID is fixed at image build time, so data volumes stay readable across rebuilds. When the entrypoint already runs unprivileged, `LookupNominatimUser` returns the current UID and GID, and children run as that user.

`Runner.Command` sets `SysProcAttr.Credential` only when the entrypoint runs as root and the target UID is not 0, because the kernel permits the switch only then. The empty, non-nil `Groups` slice makes Go call `setgroups`. Without it, the child keeps root's supplementary groups, including gid 0 and any compose `group_add` group.

`NewRunner` in `internal/ctl/serve.go` chowns `$HOME` on every start. Under a read-only root filesystem, `$HOME` is a tmpfs that the runtime mounts fresh and root-owned on each boot.

### Child environment

`BaseEnv` in `internal/ctl/serve.go` builds the environment of every child: a fixed base (`PATH`, `HOME`, UTF-8 locale, unbuffered Python) plus every inherited `NOMINATIM_*` and `PG*` variable. Operators use these to tune Nominatim settings and to supply libpq TLS material.

Three variables match the `NOMINATIM_` prefix but are never passed on. `NOMINATIM_DATABASE_DSN` belongs to the entrypoint, which appends its own value last. `NOMINATIM_PASSWORD` and `NOMINATIM_WEBUSER_PASSWORD` must not reach a child: Gunicorn connects as the web role and must not hold the owner's password.

`HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`, in both cases, are passed on because pyosmium fetches the replication diffs and must use the same network path as the Go downloader. `ALL_PROXY` is deliberately left out: Go's `net/http` ignores it, so passing it would let pyosmium and the downloader disagree.

`Runner.WithEnv` appends entries, and the later of two duplicates wins. `runGunicorn` uses this to swap in the web role's DSN, and `warmCaches` uses it to relax timeouts.

With `DEBUG_MODE`, `Serve` logs the rendered env file but withholds the `NOMINATIM_DATABASE_DSN` line. Redaction cannot cover it: `RegisterSecret` in `internal/ctl/log.go` ignores values shorter than four characters, so a short password would print in clear.

### Output and signals

`Runner.Command` wraps child stdout and stderr in `RedactWriter`, so child output passes the same secret masker as the entrypoint's own logs. `Runner.Run` flushes both writers after the child exits.

Cancelling a child's context sends SIGTERM, not SIGKILL, so Gunicorn drains and an import is not cut mid-transaction. `WaitDelay` escalates to SIGKILL after `Runner.Grace`, which is `Config.ShutdownGrace`: the Gunicorn graceful timeout plus 5 seconds.

### Serve-side replication

`startReplication` in `internal/ctl/serve.go` decides whether serve runs a background replication process:

- Without `REPLICATION_URL`, there is no replication.
- A frozen database cannot take updates: freezing removes the update tables, so every diff would fail. The database state decides this, not the `FREEZE` setting. Serve then runs without replication and logs a warning.
- `nominatim replication` shells out to osm2pgsql for every diff, and the serve-only image does not ship it. An explicit `UPDATE_MODE` there is an error rather than a silently stale API; without `UPDATE_MODE`, replication is skipped.
- `nominatim replication --init` runs on every start, so changed replication settings take effect, and the state stays usable for a manual `nominatim replication --once`. Without `UPDATE_MODE`, serve stops after `--init`.
- If `REPLICATION_URL` is unreachable at start, the API is served anyway, because an upstream outage must not take the API down. A goroutine re-checks every `replicationRetry` (1 minute) and launches the process once the URL answers.

The returned channel has a buffer of one and receives exactly one value: the process exit status, a launch error, or, when the context is cancelled before launch, the context error. The last case exists so that the shutdown wait in `runGunicorn` returns at once instead of timing out.

`replicationArgs` passes `--threads`, which bounds the indexing connections, as in the import.

### runGunicorn

`runGunicorn` in `internal/ctl/serve.go` runs Gunicorn in the foreground with the web role's DSN. It sets `NOMINATIM_QUERY_TIMEOUT` to 10 seconds and `NOMINATIM_REQUEST_TIMEOUT` to 60 seconds unless the operator sets them.

The supervision loop waits for Gunicorn to exit or for a replication result:

- A replication result that arrives after the container was asked to stop is ignored.
- `once` and `catch-up` replication are expected to finish. The result is logged, and serving continues.
- Continuous replication is meant to run for good. If it exits, Gunicorn is stopped and `runGunicorn` returns an error, so the container exits non-zero and the restart policy brings both back. Serving on without it would let the data go stale unnoticed.
- After a replication result arrives, `replicationDone` is set to nil. A nil channel disables that `select` case, and it tells the shutdown path that no further value will come.

A replication error's text leaves the process only through `Logf`, or through the error returned to `main`, which prints it with `Errf`; both redact secrets.

On shutdown, `runGunicorn` cancels replication and waits up to `Runner.Grace` plus 1 second for its exit status. SIGTERM lets the current diff finish. A cancelled context returns nil, so `docker stop` exits 0. A Gunicorn exit without a stop request returns an error, so a crash exits non-zero.

### Updater container

`Replicate` in `internal/ctl/serve.go` is the updater container of a split deployment. `UPDATE_MODE` defaults to `continuous`; `once` and `catch-up` exit when done, and the restart policy governs `continuous`. `FREEZE` is ignored, because with `REPLICATION_URL` set the import never froze the database.

The updater probes `POSTGRES_DB` itself as the `nominatim` role, because that role may be kept out of the maintenance database. A missing database ends the wait at once with a "run import first" error. It requires the import marker and refuses a frozen database. Unlike serve, an unreachable `REPLICATION_URL` is an error: the container exits non-zero and the restart policy retries.

### Healthcheck

`Healthcheck` in `internal/ctl/serve.go` probes `/status.php?format=json` in-process, so the image needs no curl. Nominatim answers HTTP 200 even when it cannot reach its database; only the JSON body says so, with `"status": 700`. A status-code check alone would keep a broken API in rotation, so only `"status": 0` counts as healthy. The probe times out after 10 seconds and reads at most 64 KiB of body.

## Secret masking

### Registering secrets

`RegisterSecret` in `internal/ctl/log.go` adds a value to the masker. Values shorter than four characters are ignored: masking them would garble ordinary log text. `Redact` replaces every registered value with `***`; `Logf` (stdout) and `Errf` (stderr) always redact.

`RegisterURLSecrets` covers `PBF_URL`, `REPLICATION_URL` and `DATA_MIRROR_URL`, which are logged on every download:

- userinfo: the password of `https://user:pass@…`, or the whole userinfo of a bare-token URL `https://TOKEN@…`;
- query values of presigned or SAS URLs whose key is in the exact list in `credentialParam` (`sig`, `x-amz-signature`, `token`, `key`, …). Exact names, because a substring test would also catch `X-Amz-SignedHeaders` or `passive`. Values shorter than eight characters are skipped, because flag-like values (`host`, `true`) would mask common words in every line.

Each credential is registered both as written and percent-decoded. URLs are logged as written, so `rawURLPassword` extracts the userinfo secret from the raw string, percent-encoding included, splitting at the last `@` before the path.

### Child process output

`RedactWriter` filters a child's stdout and stderr through the same masker, so a traceback or driver error cannot echo a DSN into the container log. It buffers until a line terminator, so a secret split across two `Write` calls is still masked. Both `\n` and a bare `\r` end a line, and `\r\n` counts as one terminator: osm2pgsql reports progress with bare `\r`, and splitting on `\n` alone withheld that output until the run finished. A partial line longer than 1 MiB is flushed as is, so the buffer cannot grow without bound. `Flush` writes the trailing partial line when the child exits.

## Image build

### Stages and pins
The Dockerfile has six stages: `go-build` (the entrypoint binary), `base` (Debian), `py-build` (the Python venv), `serve-base` (everything the API needs except the binary), `serve` and `full`.
The `# syntax=` line, both base images and every `FROM` are pinned by digest, and Dependabot bumps them.
The Makefile reads the `go-build` and `base` `FROM` lines with `sed`, so each must stay on one line ending in `AS go-build` or `AS base`. Those lines are the only place the pins live.

### Layer order
`serve-base` holds all apt and user setup. The `serve` and `full` stages copy the Go binary as their last layer, so a Go change never re-runs an apt layer.
`osm2pgsql` and `psql` exist only in `full`. `nominatim import` runs `osm2pgsql`, and `nominatim replication` shells out to it for every diff, so both capabilities need the full image.

### Python environment (`py-build`)
The environment is a venv at `/opt/nominatim`, so it copies into later stages as one directory.
It uses `--system-site-packages` to pick up PyICU from Debian's `python3-icu`. PyICU ships no wheel, and compiling it under QEMU on the arm64 leg is why the image installs no compiler at all.
Debian's pip installs the hash-checked `requirements.txt` and is then uninstalled. There is no pip upgrade, because that would be an unpinned download, and the runtime image keeps no installer.

### apt
Each apt layer deletes `docker-clean` and writes `Binary::apt::APT::Keep-Downloaded-Packages "true"`, so the `/var/cache/apt` and `/var/lib/apt` cache mounts keep their `.deb` files between builds.
Recommends and suggests are disabled.
Packages are not version-pinned, hence `# hadolint ignore=DL3008`. The base image is digest-pinned and a scheduled rebuild picks up Debian fixes (see "Known gaps" in docs/REFACTOR.md).

### User
The `nominatim` user is written straight into `/etc/passwd`, `/etc/group` and `/etc/shadow`, because the `useradd` package ships five setuid-root binaries.
`NOMINATIM_UID` and `NOMINATIM_GID` are fixed at 1000, so a rebuilt image keeps working with existing data volumes.
The entrypoint starts as root only to take ownership of the mounted volume. Every workload process then runs as `nominatim` through a direct setuid.

### setuid stripping
A `RUN` in `serve-base` strips every setuid and setgid bit and fails the build if any remains. The entrypoint needs none of them, so they are only an escalation surface.
The `full` stage repeats the strip and the assertion, in case a package it installs ships such a binary.
`integration.sh` `security` asserts the result at runtime.

### `full` package set
`postgresql-client` looks unused, because this repo talks to PostgreSQL through `pgx`. But `nominatim import` pipes `country_osm_grid.sql.gz` and the Wikipedia importance dumps into a `psql` subprocess, so removing it breaks every import.
`osm2pgsql-gen` is never invoked by Nominatim. It is what pulls in OpenCV, GDAL, Mesa and LLVM, about 195 MB that nothing links against.
The `full` layer deletes `/usr/bin/osm2pgsql-gen` and runs `dpkg --purge --force-depends` on those libraries. The purge must happen in the same layer as the install, or the bytes stay in an earlier layer.
The library names carry sonames, such as `libgdal36` and `libllvm19`. After a base image bump, check that the list still matches what apt installs.
The `osm2pgsql --version` line right after the purge fails the build, not production, if a future package change makes `osm2pgsql` need any purged library.

### Healthcheck
The healthcheck runs `nominatim-ctl healthcheck` every 30 s with a 10 s timeout and 3 retries. Its start period is 48 h, because a planet import can run for days before the API answers and must not be reported unhealthy meanwhile.

## Make targets

### Containers only
Every target runs in a container and nothing is installed on the host.
`GO_IMAGE` and `BASE_IMAGE` come from the Dockerfile `FROM` lines (see "Stages and pins"), so local runs and CI use the same digest-pinned images.
`lint` runs shellcheck and hadolint from digest-pinned images, and CI's `static` job runs the same target.

### Cache volumes and ownership
The Go module cache and build cache are the named volumes `nominatim-gocache-mod` and `nominatim-gocache-build`, mounted under `/gocache` and selected through `GOMODCACHE` and `GOCACHE`. Without them every run recompiles the world.
Containers run as the invoking user (`-u $(UID):$(GID)`), so files written into the mount keep host ownership.
A fresh named volume is root-owned. The `gocache` target chowns both volumes once and is a prerequisite of `tidy`, `fmt`, `fmt-check`, `vet`, `test` and `vuln`.
`clean` removes both volumes.

### Other targets
`help` greps the `## text` after each target name, so every documented target needs one.
`check` is the fast gate: `vet`, `test` and `fmt-check`.
`requirements` resolves `requirements.txt` on the same Debian base the image builds on, so the pins match what the image installs. It excludes `pyicu`, which has no wheel and comes from Debian's `python3-icu`.
`vuln` pins `govulncheck` to an exact version.

### Ignored files
`.env` is ignored because it holds local secrets for the `contrib` compose stacks. `*.log` is ignored for container logs captured while debugging.

## CI

### Triggers and concurrency
`ci.yml` runs on push, on pull request, manually, and weekly (Monday 00:23 UTC). The weekly run rebuilds and republishes without cache, so Debian security fixes reach `:latest` and `:serve`.
`vuln.yml` runs on the same triggers plus Monday 04:23 UTC, after the weekly rebuild, so it scans the fresh `:latest`. It is scheduled because a dependency can become vulnerable without any commit here.
`cancel-in-progress` is true only for pull requests. Cancelling a push run could stop `publish` between its registry pushes and leave the tags inconsistent.

### `static`
It runs `make vet test fmt-check lint`. The Makefile holds the pinned golang, hadolint and shellcheck images, so CI and local runs cannot drift apart.

### `build`
It is skipped only when the head commit message contains the bracketed `[skip_build]`. A bare `skip_build` matched anywhere in the message, including the body. A commit that merely mentioned the flag then skipped the whole matrix, and the run stayed green because skipped jobs do not fail. That hid a broken CI from 2026-04-22 onward.
The version comes from `ARG NOMINATIM_VERSION` in the Dockerfile.
The full image is exported as a tar for the `test` job. Its GHA cache (scope `full`) is read back by `publish`, so the amd64 layers pushed are the ones tested.
The `serve` target is built only to prove it still builds. Its behaviour is asserted by the `integration` job (`integration.sh` `serve_image`). It has its own cache scope, `serve`.
On a schedule `no-cache` is on, because cached apt layers never pick up Debian security fixes.

### `test` matrix
Each scenario runs on its own runner, because `start-postgres` and `run-nominatim` use fixed container names and port 8080, so nothing needs cleaning up. Scenarios that import Monaco use `monaco-latest` unless noted.
- Import with persistent data and container restart: imports a PBF from four days ago, so replication really has updates to apply, then stops and removes the container and starts it again on the same database and the `nominatim-data` volume. The second start must log `existing Nominatim import found` and reuse the import. A `nominatim replication --once` then runs as the `nominatim` user with an explicit owner DSN, because the image's `.env` holds the read-only web DSN. This scenario also covers a plain `PBF_URL` import followed by an update.
- `UPDATE_MODE=once` and `UPDATE_MODE=continuous`: same four-day-old PBF, and wait for `Update completed.` in the container log. Replication output goes to the container's stdout, not to a log file inside the container.
- Import admin style: asserts the opposite of the default style, so a POI search (`hotel de paris`) is empty while the Monaco boundary is still found. The status check comes first, because the first non-empty `assert-json` call has the longest retry budget and covers the import.
- Import with `PBF_PATH`: downloads the PBF on the runner and mounts it at `/nominatim/data`.
- `FREEZE`, `REVERSE_ONLY` and `WARMUP_ON_STARTUP=true` (waits for `Warming finished`).
- GB postcode import: imports Rutland. `PE9 3SY` was retired from the ONS dataset and is absent from the current `gb_postcodes.csv.gz`, so the test uses `PE9 3AL`, the nearest surviving `PE9 3*` postcode.
- Check for clean shutdown: starts detached, then `docker stop -t 60` and an exit code of 0. It also covers a blank `REPLICATION_URL` (log `Skipping cache warmup`) and the `WARMUP_ON_STARTUP=false` default.
- API endpoints coverage: runs the default (full) style with a volume-mounted project dir and checks `/status`, `/search` (including `addressdetails`, `polygon_geojson` and structured queries), `/reverse`, `/lookup` and `/details` against Monaco, plus the `Content-Type` header and `nominatim admin --check-database`.
The `Content-Type` check uses GET, not HEAD. Nominatim answers HEAD with a 405 whose error body is JSON too, so a HEAD check would pass vacuously.

### `integration`
It runs `test/integration.sh`, which builds both the full and serve targets itself. It is the only CI coverage of the serve image and of the shipped split compose stack.

### `publish`
It needs `static`, `test` and `integration`, and runs for push and schedule events on `master` or `test`. Schedules run only on the default branch, so they publish the `master` tags. The `test` branch publishes the same images with a `-test` suffix for pre-merge validation.
The weekly rebuild moves only the rolling tags (`latest`, `serve`). Commit tags (`v<version>-<sha7>`) are written on push only, so a commit tag keeps the image that commit was tested as.
Each push reads the build job's cache. On a hit, the amd64 image pushed is layer for layer the one the tests ran. arm64 is built here and never tested.
The repository name is lowercased because OCI references must be lowercase.
A schedule has no head commit, so its `org.opencontainers.image.created` label uses the build time.
Docker Hub steps run only for the `maxysoft` owner. GHCR steps always run.

### `vuln.yml` jobs
- `govulncheck` runs `make vuln`, which holds the pinned golang image and govulncheck version.
- `lockfiles` runs `osv-scanner` on `requirements.txt` and `go.mod` with `--no-call-analysis=go`. Go reachability is the `govulncheck` job's job, and the scanner image has no Go toolchain new enough for `go.mod`. This scan also flags `go.mod` drift when a golang bump has not raised the `go` directive.
- `image` runs on schedule and manual dispatch only. It scans `:latest`, not `:serve`, because `:serve` is `serve-base` plus one binary and `:latest` holds every package. It fails only on vulnerabilities that have a fix available. Findings under `/usr/lib/python3/dist-packages/` are ignored: Debian patches its `python3-*` libraries under the upstream version, so PyPI fix versions never apply there, and their Debian 13 entries cover them.

### Dependabot
It checks GitHub Actions, Go modules, the Dockerfile and the compose files in `/contrib` and `/test`, weekly, with one grouped PR per ecosystem.
A golang image bump needs the `go` directive in `go.mod` raised by hand.
The compose ecosystem ignores `nominatim` and `nominatim-itest`, which are built locally and exist in no registry.

### Workflow helper scripts
They live in `.github/workflows/` and the `test` job runs them from that directory.
- `start-postgres` creates the `nominatim-net` network and starts the pinned PostGIS container as `postgres`, with no volume because no scenario restarts PostgreSQL. It waits up to 120 s for `pg_isready`, the same cadence as the compose healthcheck, and prints the container log on failure. Usage: `start-postgres`.
- `run-nominatim [docker run flags ...]` runs `nominatim:$NOMINATIM_VERSION` on that network with the settings every scenario shares. Scenario flags (`-e`, `-v`, `-d`) go first. Container name and port are fixed, because each matrix leg has its own runner. It omits `--rm` because the shutdown scenario inspects the exit code and the restart scenario removes the container itself.
- `wait-log PATTERN [TRIES]` polls the container log every 5 s (default 60 tries) for a fixed string and prints the last 50 log lines on failure. It polls because `docker logs -f | grep -m1` reports the timeout's exit status even when it matches.
- `assert-json` is stdlib-only Python, because the runner's `python3` has no third-party packages. It retries refused connections and 502, 503 and 504 with exponential backoff (2 s doubling, capped at 120 s) and exits non-zero on any other failure. The non-empty form is each scenario's first call, so it allows 19 attempts to cover the import itself. The field and `--empty` forms allow 8. Nominatim reports failures with HTTP 200, for example reverse answers `{"error":"Unable to geocode"}` and `status.php` answers `{"status":700}` when the database is down, so the non-empty form also rejects an `error` key or a non-zero `status`.

## Integration tests

### Running
`make integration` runs `test/integration.sh [scenario ...]`, with all scenarios by default. It builds `nominatim-itest:local` first, then runs the scenarios in order, which share one compose stack. `ITEST_PORT` (default 18080) moves the API port, because 8080 is often taken on a dev host. The serve container uses `ITEST_PORT+1` and the split stack uses `ITEST_PORT+2`.
An EXIT trap removes the containers and volumes of the compose stack, the serve container and the split stack. The passwords are exported once so that trap can interpolate the compose files too.
The helpers capture command output before grepping. Piping into `grep -q` trips `pipefail` with SIGPIPE (rc 141) when grep matches early.
`status_ok` checks the body for `"status":0`, because Nominatim answers 200 with `{"status":700}` when it cannot reach the database.

### Scenarios
- `full`: imports Monaco and checks status, search, reverse, lookup, details, the JSON `Content-Type` and `nominatim admin --check-database`. The `Content-Type` check uses GET, because Nominatim answers HEAD with a 405 whose error body is JSON too.
- `security`: the `nominatim` role is not a superuser, the `www-data` role exists and the API is connected as it, no setuid or setgid binaries exist (setgid is checked too, because the Dockerfile strips both), `sudo`, `sshpass`, `scp`, `ssh` and `curl` are absent, `/nominatim/.env` is mode 600 with no `__` placeholder left, and Gunicorn runs non-root. The UID is read from `/proc` inside the container, because `docker top` resolves UIDs against the host passwd file.
- `restart`: the `placex` row count is unchanged across a restart and the log reports `skipping import`. It polls the log, because `docker compose logs` lags briefly behind the container. Then it clears the database comment marker and runs `import` with `ALLOW_DROP_EXISTING_DB=true`. A valid import without the marker (a `pg_restore` into a fresh database) must be validated and adopted, never dropped.
- `volume_loss`: removing the project volume does not drop a populated database.
- `serve_image`: builds the `serve` target, checks that `osm2pgsql` and `psql` are absent, and checks that it refuses to import against a database with no import, failing fast with `serve-only` before downloading anything. Then it serves the existing import on a read-only root filesystem, as the shipped compose files run it. The project dir is a named volume, not a tmpfs, because volumes seed ownership from the image while tmpfs mounts come up root-owned.
- `shutdown`: `compose stop` exits 0 within 20 s.
- `failfast`: a missing `NOMINATIM_PASSWORD`, an invalid `UPDATE_MODE` and a rejected admin password each exit non-zero with a diagnostic. The wrong-password case must fail in seconds, not after the connection budget.
- `unicode_password`: a password with a soft hyphen (mapped away), a no-break space (mapped to a space) and multi-byte characters authenticates only if our SASLprep matches the server's, and no unit test can establish that. The cleartext must not appear in the log. The scenario cleans up at the end because later scenarios use a different password.
- `drop_guard`: a database holding its own tables and no completion marker is refused and keeps its data. A `nominatim` role that cannot connect makes validation impossible, which must never read as invalid, even with `ALLOW_DROP_EXISTING_DB`. While another session holds the advisory import lock, a second container waits and decides nothing. The lock key must match `importLockClass` in `internal/ctl/db.go`. The scenario talks to PostgreSQL over TCP, because the image's temporary init server listens on the socket only, so TCP reaches the final server.
- `split`: runs `contrib/docker-compose-local.yml` (project `nominatim-itest-split`, profile `updates`). The one-shot import exits 0 after importing. The API container depends on `service_completed_successfully`, has no `osm2pgsql` or `psql`, and holds no admin password. A second `up` of the import service skips the completed import and leaves the row count unchanged. The updater initialises replication and keeps running.

### Test compose file
`test/docker-compose.test.yml` is the local integration stack. `integration.sh` drives it, and it is not for production (see `contrib/` for deployment examples).
PostgreSQL runs with `fsync=off`, `full_page_writes=off` and `max_connections=100` for speed. Its healthcheck polls every 3 s, up to 30 times.
The nominatim service is hardened like the shipped stacks: `init: true` reaps orphaned processes, because the entrypoint deliberately does not. `no-new-privileges` is safe to assert because the image ships no setuid binary. The root filesystem is read-only, because nothing is written outside `/nominatim` (the volume), `/tmp` and `/var/lib/nominatim` (tmpfs) and `/dev/shm`. All capabilities are dropped except `KILL`, `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `SETUID` and `SETGID`.
`PBF_URL`, `ITEST_PASSWORD`, `IMPORT_STYLE`, `REVERSE_ONLY` and `WARMUP_ON_STARTUP` can be overridden from the environment.
The API port is bound to `127.0.0.1`.

## Unit tests

Test files carry no comments. This section records what the less obvious tests guard.

### Helper constraints

- Tests that call `t.Setenv` mutate process environment and must not call `t.Parallel`. `TestServeOnlyImageGuards` and `TestReplicateGuards` in `internal/ctl/run_test.go` set `PATH` to an empty temp directory to simulate the serve-only image, which has no osm2pgsql.
- Secrets registered by any test (`RegisterSecret`, `RegisterURLSecrets`) stay in the process-global set for the rest of the run, and `Redact` applies them in registration order. A new test's secret must not occur inside another test's secret or inside text another test expects to see unmasked.

### Configuration (`internal/ctl/config_test.go`)

- Validation tests assert that a missing application password is an error: the previous image baked a default into the Dockerfile and used it as the PostgreSQL superuser password. The admin password must never be inferred from the application password, and a separate web-role password is what actually separates the privileges.
- With no import settings, `Load` must still succeed: a restart of an already-imported database must not require the original PBF settings.
- Dataset values that are relative or misspelled must be rejected. The shell version skipped them silently and produced an import missing its importance data.
- The DSN tests are parsed by pgx, not `net/url`. pgx follows libpq: userinfo ends at the first `@` and `+` stays literal. A mis-escaped password is set correctly on the role and then fails every later login. The characters rejected in passwords are field separators in Nominatim's own `pgsql:` DSN parser.
- An IPv6 host literal must be bracketed in the URL, or the URL does not parse.
- `.env` rendering tests guard four old failures: placeholders consumed on first run so a persisted volume ignored later environment changes; a line appended on every start that grew the file without bound; an unset replication URL leaving the literal `__REPLICATION_URL__` placeholder; and an unanchored sed splicing intervals (86400 became 864000, then "3000"). A render must be stable and must fully replace a previous render.
- `.env` sits in a directory the workload user can write to and is rewritten as root on every start. A symlink there must be replaced, never followed. The file is readable by the API uid, so it must not hold the owning role's password.
- The Gunicorn timeout settings must be numeric: a non-number killed Gunicorn after "ready" was logged. The graceful timeout also drives the SIGKILL deadline (`TestRunnerGraceFollowsConfig` in `internal/ctl/run_test.go`); a fixed 35 s cut a longer drain short.
- `verifyChecksum` accepts hex digits in either case and nothing else. A pasted `sha256sum` line used to fail only after the whole download, and the restart policy then repeated the download forever.

### Database (`internal/ctl/db_test.go`)

- A wrong password must surface at once. It used to be retried for the whole five-minute budget and reported as a generic "not reachable".
- Only a database proven missing may be read as empty. Other errors (for example `53300` too_many_connections, `28000` rejected by pg_hba) must not reach the import path, where they could end in `DROP DATABASE` on a finished import.
- `NOSUPERUSER` contains the substring `SUPERUSER` and must not switch provisioning off. Removing `SUPERUSER` from `NOMINATIM_ROLE_OPTIONS` must revoke it, and only a superuser role gets `NOSUPERUSER` because issuing it needs superuser.
- The quoting tests use injection strings. An unescaped password reaching `ALTER ROLE` was arbitrary SQL as the PostgreSQL superuser. An unquoted `${POSTGRES_DB}` folded case (dropping the wrong database) and allowed a second statement.
- The drop statement uses `FORCE` because a connection left by a previous container made re-import fail with "is being accessed by other users".
- The SCRAM verifier test is cross-checked against an independent implementation (Python hashlib/hmac), not this package's own output, so an algorithm mistake cannot pass alongside itself. The cleartext password must not appear in the statement for any password, not only printable ASCII. A fresh salt per call makes two verifiers differ for one password.
- The SASLprep cases are the RFC 4013 worked examples plus a few NFKC ones: soft hyphen is removed, case is kept, no-break space becomes a space, ordinal indicator and roman numeral nine normalise. PostgreSQL applies the same transformation before hashing. When SASLprep rejects input (for example a BEL control character), PostgreSQL falls back to the raw bytes and so must this package.
- PostgreSQL, pgx (`precis.OpaqueString`) and this package must derive the same key. The code hands pgx a pre-prepared password, which works because SASLprep output is a fixed point. The connection URL must carry the prepared form, or pgx and the server derive different keys for a non-ASCII password.

### Downloads (`internal/ctl/download_test.go`)

- A wrong checksum must not leave the bad file behind for a later run to use. A completed file that no longer matches an updated checksum is fetched again in the same run rather than failing the import.
- Resume tests protect multi-GB planet downloads. A mid-transfer failure must not discard the work, and a partial of exactly the remote length (killed before completion was recorded) is finished, not downloaded again.
- Resume appends only when the server confirms the offset requested. A server that lies about the start offset, or ignores `Range`, must not have its response appended. The lying-server test returns a different starting offset on purpose.
- A partial left by another URL or an older upstream version is restarted from scratch. Without a validator no `If-Range` is sent, so the server would splice two versions.
- The new version's state is written only once the destination is truncated for it, so a failure in between keeps the old bytes under the old state. The test forces the truncating open to fail by making the destination a directory.
- A dataset path that is a symlink (from an earlier `IMPORT_X=/path` run) is replaced, not written through into the file it points at.
- A connection that stops sending body bytes must be abandoned and resumed. The stall test holds the connection open until the request context ends.
- A completed download of the same URL is reused, not fetched again.

### Logging (`internal/ctl/log_test.go`)

- `DEBUG_MODE` used to enable `set -x` and echo every password into container logs. Everything the process prints is now filtered, including child output, since a Nominatim traceback or osm2pgsql error can echo a DSN.
- The filtering writer buffers to line boundaries so a secret split across two `Write` calls is still caught. Very short secrets are ignored because they would match unrelated text.
- osm2pgsql reports progress with a bare `\r`. Splitting on `\n` alone buffered the whole progress stream, so a 21-minute import showed nothing and then dumped one huge line. CRLF must not produce an empty line, and a final line without a newline must not be lost.
- URL secrets: mirror credentials are redacted in both raw and percent-decoded form. A bare userinfo token or a presigned query value is the credential even without a `user:pass` pair. Harmless query values stay readable, and a non-credential key that merely contains "sig" (`X-Amz-SignedHeaders`) or a short value must not be redacted.

### Import (`internal/ctl/import_test.go`)

- `IMPORT_X=true` over a file the operator placed at the destination must neither truncate it nor delete it on a checksum mismatch.
- A dataset the operator placed where Nominatim looks for it is used in place. Deleting it and linking the destination to itself made the import skip it. After the import only downloads and symlinks are removed.
- With a dataset unset, leftovers of an earlier failed run must be removed before the import or Nominatim imports them anyway. A file of the operator's stays.

### Run modes (`internal/ctl/run_test.go`)

- The serve-only image must refuse an import before any download or provisioning, refuse an explicit `UPDATE_MODE`, and silently skip replication otherwise. `FREEZE` must not skip replication the import set up.
- The foreground updater refuses before touching the database when there is no URL or osm2pgsql is missing. `FREEZE` does not stop it: the import lets `REPLICATION_URL` win, so the database was never frozen.

### Serve (`internal/ctl/serve_test.go`)

- Gunicorn connects as the web role on purpose. Inheriting `NOMINATIM_PASSWORD` from the container environment would hand it the CREATEDB role.
- `status.php` answers 200 even when the database is unreachable, so the healthcheck must read the body, or a broken API stays healthy.
