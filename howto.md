# Nominatim Docker (Nominatim version 5.3)

> **Note:** This version has been modified to use external PostgreSQL/PostGIS instead of running PostgreSQL inside the container. For setup instructions with external database, see [EXTERNAL-POSTGIS.md](docs/EXTERNAL-POSTGIS.md).

## Automatic import

With an external PostgreSQL/PostGIS server reachable (see [EXTERNAL-POSTGIS.md](docs/EXTERNAL-POSTGIS.md)),
download the required data, initialize the database and start nominatim in one go:

```sh
docker run -it \
  -e PBF_URL=https://download.geofabrik.de/europe/monaco-latest.osm.pbf \
  -e REPLICATION_URL=https://download.geofabrik.de/europe/monaco-updates/ \
  -e POSTGRES_HOST=your_postgres_host \
  -e NOMINATIM_PASSWORD=very_secure_password \
  -e POSTGRES_ADMIN_PASSWORD=your_postgres_password \
  -p 8080:8080 \
  --name nominatim \
  ghcr.io/maxysoft/nominatim-docker:latest
```

Port 8080 is the Nominatim HTTP API port. PostgreSQL runs on your external server, not in this container.

If you want to check that your data import was successful, you can use the API with the following URL: <http://localhost:8080/search?q=avenue%20pasteur>

## Configuration

### General Parameters

The following environment variables are available for configuration:

- `PBF_URL`: Which [OSM extract](#openstreetmap-data-extracts) to download and import. It cannot be used together with `PBF_PATH`.
  Check [https://download.geofabrik.de](https://download.geofabrik.de)
  Since the download speed is restricted at Geofabrik, there is a recommended list of mirrors for importing the full planet at [OSM Wiki](https://wiki.openstreetmap.org/wiki/Planet.osm#Planet.osm_mirrors).
  At the mirror sites you can find the folder /planet which contains the planet-latest.osm.pbf
  and often a `/replication` folder for the `REPLICATION_URL`.
- `PBF_PATH`: Which [OSM extract](#openstreetmap-data-extracts) to import from the .pbf file inside the container. It cannot be used together with `PBF_URL`.
- `REPLICATION_URL`: Where to get updates from. For example Geofabrik's update for the Europe extract are available at `https://download.geofabrik.de/europe-updates/`
Other places at Geofabrik follow the pattern `https://download.geofabrik.de/$CONTINENT/$COUNTRY-updates/`

- `REPLICATION_UPDATE_INTERVAL`: How often upstream publishes diffs (in seconds, default: `86400`). _Requires `REPLICATION_URL` to be set._
- `REPLICATION_RECHECK_INTERVAL`: How long to sleep if no update found yet (in seconds, default: `900`). _Requires `REPLICATION_URL` to be set._
- `UPDATE_MODE`: How to run replication to [update nominatim data](https://nominatim.org/release-docs/5.3/admin/Update/#updating-nominatim). Options: `continuous`/`once`/`catch-up` (default: unset, so no automatic updates run)
- `FREEZE`: Freeze database and disable dynamic updates to save space. Ignored when `REPLICATION_URL` is set; a database frozen earlier stays frozen and is served without updates. (default: `false`)
- `REVERSE_ONLY`: If you only want to use the Nominatim database for reverse lookups. (default: `false`)
- `IMPORT_WIKIPEDIA`: Whether to download and import the Wikipedia importance dumps (`true`) or path to importance dump in the container. Importance dumps improve the scoring of results. On a beefy 10-core server, this takes around 5 minutes. (default: `false`)
- `IMPORT_SECONDARY_WIKIPEDIA`: Whether to download and import the Wikipedia secondary importance dumps (`true`) or path to secondary importance dump in the container. (default: `false`)
- `IMPORT_US_POSTCODES`: Whether to download and import the US postcode dump (`true`) or path to US postcode dump in the container. (default: `false`)
- `IMPORT_GB_POSTCODES`: Whether to download and import the GB postcode dump (`true`) or path to GB postcode dump in the container. (default: `false`)
- `IMPORT_TIGER_ADDRESSES`: Whether to download and import the Tiger address data (`true`) or path to a preprocessed Tiger address set in the container. (default: `false`)
- `DATA_MIRROR_URL`: Base URL the supplementary datasets above are downloaded from over HTTPS (default: `https://nominatim.org/data`).
  Point it at your own mirror to avoid loading the upstream servers (see [#416](https://github.com/mediagis/nominatim-docker/issues/416)).
- `IMPORT_WIKIPEDIA_SHA256` (and the same suffix on every other `IMPORT_*` switch), `PBF_SHA256`: Optional SHA-256 of the download, verified before it is used. (default: unset)
- `USER_AGENT`: User-Agent sent with every download. (default: `maxysoft/nominatim-docker:<version>`)
- `THREADS`: How many threads should be used to import (default: the container's CPU allowance, which is the cgroup CPU quota when one is set and all cores otherwise)
- `GUNICORN_WORKERS`: Specifies how many Gunicorn worker processes should handle API requests. If not explicitly set, it defaults to the container's CPU allowance (same rule as `THREADS`). Increase this value to improve concurrent request handling capacity, but ensure it aligns with your server's CPU resources. Each worker holds up to `NOMINATIM_API_POOL_SIZE` database connections (5 by default), so keep `GUNICORN_WORKERS` × 5 plus `THREADS` below PostgreSQL's `max_connections`.
- `GUNICORN_BIND`: Address the API listens on; the healthcheck follows it. (default: `0.0.0.0:8080`)
- `GUNICORN_TIMEOUT`, `GUNICORN_GRACEFUL_TIMEOUT`: Request and shutdown drain deadlines in seconds. (default: `60`, `30`) Raising the drain deadline also needs the compose `stop_grace_period` (40 s) raised to at least that plus 5 s.
- `POSTGRES_HOST`: Hostname or IP of the PostgreSQL server. (default: `postgres`)
- `POSTGRES_PORT`: Port of the PostgreSQL server. (default: `5432`)
- `POSTGRES_DB`: Name of the database. (default: `nominatim`)
- `POSTGRES_SSLMODE`: Any libpq `sslmode`; use `require` or stricter in production. (default: `prefer`)
- `NOMINATIM_PASSWORD`: Password for the `nominatim` application role, which owns the database. **Required. There is no default.**
  Use `NOMINATIM_PASSWORD_FILE` to read it from a secret file instead. Must not contain `;` or `=`.
- `NOMINATIM_WEBUSER_PASSWORD`: Password for the read-only `www-data` role the API connects as. Falls back to
  `NOMINATIM_PASSWORD` with a warning; the shipped compose files require it. Same rules, and a `_FILE` variant.
- `POSTGRES_ADMIN_PASSWORD`: Password for the PostgreSQL superuser. **Required for the initial import**,
  used only to create roles and install PostGIS. Also supports a `_FILE` variant.
- `NOMINATIM_WEBUSER`: Name of the read-only role the API connects as. (default: `www-data`)
- `NOMINATIM_ROLE_OPTIONS`: Attributes of the `nominatim` role. Set `SUPERUSER` only if your provider cannot pre-install extensions. (default: `CREATEDB`)
- `PROVISION_EXTENSIONS`: Install missing PostGIS and hstore extensions into `template1`. Set `false` on a shared cluster and install them yourself. (default: `true`)
- `ALLOW_DROP_EXISTING_DB`: Let an import drop a database that already holds tables. Prefer the `reimport` subcommand (see [Docker Compose](#docker-compose)). (default: `false`)
- `WARMUP_ON_STARTUP`: Whether to warm up the database caches on container startup by loading tables and indices into RAM. This can improve initial query performance, especially on systems with slow disks and sufficient RAM. However, it will increase the container's startup time. Set to `true` to enable. (default: `false`)
- `DEBUG_MODE`: Enable verbose debug output showing all executed commands during startup and import. Useful for troubleshooting but creates noisy logs. Set to `true` to enable. (default: `false`)
- `PROJECT_DIR`: Nominatim project directory inside the container; the volume examples below assume the default. (default: `/nominatim`)

For PostgreSQL tuning, configure your external PostgreSQL server according to the [official Nominatim documentation](https://nominatim.org/release-docs/5.3/admin/Installation/#tuning-the-postgresql-database).

### Import Style

The import style can be modified through an environment variable :

- `IMPORT_STYLE` (default: `full`)

Available options are :

- `admin`: Only import administrative boundaries and places.
- `street`: Like the admin style but also adds streets.
- `address`: Import all data necessary to compute addresses down to house number level.
- `full`: Default style that also includes points of interest.
- `extratags`: Like the full style but also adds most of the OSM tags into the extratags column.

See <https://nominatim.org/release-docs/5.3/admin/Import/#filtering-imported-data> for more details on those styles.

### Flatnode files

In addition you can also mount a volume / bind-mount on `/nominatim/flatnode` (see: Persistent container data) to use flatnode storage. This is advised for bigger imports (Europe, North America etc.), see: <https://nominatim.org/release-docs/5.3/admin/Import/#flatnode-files>. If the mount is available for the container, the flatnode configuration is automatically set and used.
Add this line to the [automatic import](#automatic-import) command:

```sh
  -v nominatim-flatnode:/nominatim/flatnode \
```

## Persistent container data

When using external PostgreSQL (recommended), data persistence is handled by your external database server. For the Nominatim container, you only need to persist:

- `/nominatim` holds the Nominatim project data (configuration, replication state, optional flatnode file). Import completion is recorded in the database itself, so losing this volume does not trigger a re-import.
- `/nominatim/flatnode` is the storage location of the flatnode file (if used).

So if you want to be able to kill your container and start it up again with all the data still present, add this line to the [automatic import](#automatic-import) command:

```sh
  -v nominatim-data:/nominatim \
```

## OpenStreetMap Data Extracts

Nominatim imports OpenStreetMap (OSM) data extracts. The source of the data can be specified with one of the following environment variables:

- `PBF_URL` variable specifies the URL. The data is downloaded during initialization, imported and removed from disk afterwards. The data extracts can be freely downloaded, e.g., from [Geofabrik's server](https://download.geofabrik.de).
- `PBF_PATH` variable specifies the path to the mounted OSM extracts data inside the container. No .pbf file is removed after initialization.

It is not possible to define both `PBF_URL` and `PBF_PATH` sources.

The replication update can be performed only via HTTP.

A sample of `PBF_PATH` variable usage is:

```sh
docker run -it \
  -e POSTGRES_HOST=your_postgres_host \
  -e NOMINATIM_PASSWORD=very_secure_password \
  -e POSTGRES_ADMIN_PASSWORD=your_postgres_password \
  -e PBF_PATH=/nominatim/data/monaco-latest.osm.pbf \
  -e REPLICATION_URL=https://download.geofabrik.de/europe/monaco-updates/ \
  -p 8080:8080 \
  -v /osm-maps/data:/nominatim/data \
  --name nominatim \
  ghcr.io/maxysoft/nominatim-docker:latest
```

where the _/osm-maps/data/_ directory contains _monaco-latest.osm.pbf_ file that is mounted and available in container: _/nominatim/data/monaco-latest.osm.pbf_

## Updating the database

Full documentation for Nominatim update is available her: [Nominatim documentation](https://nominatim.org/release-docs/5.3/admin/Update/). For a list of other methods see the output of:

```sh
docker exec -it -u nominatim nominatim nominatim replication --help
```

With the shipped compose files updates run in their own container: `docker compose -f contrib/docker-compose.yml --profile updates up -d` starts `nominatim-updater`, which runs `nominatim-ctl replicate` on the full image: it initialises replication and applies diffs continuously. `UPDATE_MODE=once` or `catch-up` make that container exit when it is done.

For a single full-image container, the following command will keep updating the database forever:

```sh
docker exec -it -u nominatim nominatim nominatim replication --project-dir /nominatim
```

If there are no updates available this process will sleep for 15 minutes and try again.

## Custom PBF Files

If you want your Nominatim container to host multiple areas from Geofabrik, merge the PBF files into one with [`osmium merge`](https://osmcode.org/osmium-tool/manual.html), then set `PBF_PATH` [as above](#openstreetmap-data-extracts).

## Importance Dumps, Postcode Data, and Tiger Addresses

Including the Wikipedia importance dumps, postcode files, and Tiger address data can improve results. These can be automatically downloaded by setting the appropriate options (see above) to `true`. Alternatively, they can be imported from local files by specifying a file path (relative to the container), similar to how `PBF_PATH` is used. For example:

```sh
docker run -it \
  -e POSTGRES_HOST=your_postgres_host \
  -e NOMINATIM_PASSWORD=very_secure_password \
  -e POSTGRES_ADMIN_PASSWORD=your_postgres_password \
  -e PBF_URL=https://download.geofabrik.de/europe/monaco-latest.osm.pbf \
  -e IMPORT_WIKIPEDIA=/nominatim/extras/wikimedia-importance.csv.gz \
  -p 8080:8080 \
  -v /osm-maps/extras:/nominatim/extras \
  --name nominatim \
  ghcr.io/maxysoft/nominatim-docker:latest
```

Where the path to the importance dump is given relative to the container. (The file does not need to be named `wikimedia-importance.sql.gz`.) The same works for `IMPORT_US_POSTCODES` and `IMPORT_GB_POSTCODES`.

For more information about the Tiger address file, see [Installing TIGER housenumber data for the US](https://nominatim.org/release-docs/5.3/customize/Tiger/).

## Development

To work on the Docker image, `make build` builds it locally and `make integration` runs the local
integration suite against it (see `make help`).

## Docker Compose

In addition, we also provide a basic `contrib/docker-compose.yml` template which you use as a starting point and adapt to your needs. Use this template to set the environment variables, mounts, etc. as needed.

Every template runs Nominatim as three containers: `nominatim-import` (full image) provisions the database, imports and exits; `nominatim` (serve image) runs the API with no import tooling and no admin credentials; `nominatim-updater` (full image, `--profile updates`) applies replication diffs. Re-running `up` is safe: the import container finds the completed import, reconciles the role passwords (so a rotated `NOMINATIM_PASSWORD` takes effect) and exits. To import a newer extract into the same database run `docker compose -f contrib/docker-compose.yml run --rm nominatim-import reimport`.

The import and updater containers share the project volume (the updater needs the flatnode file when flatnode storage is in use), so they must agree on every setting that lands in `.env`; the templates keep them in one shared block. The API has its own volume.

Besides the basic docker-compose.yml, there are also some advanced YAML configurations available in the `contrib` folder.
These files follow the naming convention of `docker-compose-*.yml` and contain comments about the specific use case.

## Assorted use cases documented in issues

- [Using an external Postgres database](https://github.com/mediagis/nominatim-docker/issues/245#issuecomment-1072205751)
  - [Using Amazon's RDS](https://github.com/mediagis/nominatim-docker/issues/378#issuecomment-1278653770)
- [Hardware sizing for importing the entire planet](https://github.com/mediagis/nominatim-docker/discussions/265)
- [Upgrading Nominatim](https://github.com/mediagis/nominatim-docker/discussions/317)
- [Using Nominatim UI](https://github.com/mediagis/nominatim-docker/discussions/486#discussioncomment-7239861)
