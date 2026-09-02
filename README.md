# Nominatim Docker

100% working container for [Nominatim](https://github.com/openstreetmap/Nominatim).

![Nominatim Version](https://img.shields.io/badge/Nominatim%20Version-5.3.2-blue?style=flat-square) ![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/maxysoft/nominatim-docker/ci.yml?branch=master&style=flat-square) ![Docker Pulls](https://img.shields.io/docker/pulls/maxysoft/nominatim-docker?style=flat-square) ![Docker Image Size with architecture (latest by date/latest semver)](https://img.shields.io/docker/image-size/maxysoft/nominatim-docker?style=flat-square)

> [!IMPORTANT]  
> ⚠️ The following code modifications and implementations were generated with the assistance of **AI (Claude)**.  
> Please review carefully before using in production.

> **⚠️ Important:** This version requires an external PostgreSQL database with PostGIS. See [EXTERNAL-POSTGIS.md](docs/EXTERNAL-POSTGIS.md) for setup instructions.

> [!WARNING]
> **Base image changed from `ubuntu:24.04` to `debian:13.4-slim`** (pinned by SHA256 digest).
> My tests passed but be sure to test on your environment if you're using it in production.

## Supplementary Data

Optional supplementary datasets (Wikipedia importance dump, GB/US postcodes, Tiger addresses) are
downloaded over HTTPS from `https://nominatim.org/data`, verified against the system CA bundle.

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATA_MIRROR_URL` | `https://nominatim.org/data` | Base URL for the supplementary datasets |
| `IMPORT_WIKIPEDIA_SHA256` etc. | unset | Optional per-dataset checksum, verified after download |

Point `DATA_MIRROR_URL` at your own mirror to avoid loading the upstream servers
(see [#416](https://github.com/mediagis/nominatim-docker/issues/416)). Each `IMPORT_*` switch also
accepts an absolute path to a local file instead of `true`.

## Quick Start

The easiest way to use Nominatim Docker is by pulling the pre-built images from [Docker Hub](https://hub.docker.com/r/maxysoft/nominatim-docker) or [Github Packages](https://github.com/maxysoft/nominatim-docker/pkgs/container/nominatim-docker).

To quickly get a Nominatim instance up and running with a small dataset (e.g., Monaco):

```sh
# Use the provided docker-compose configuration
docker compose -f contrib/docker-compose.yml up
```

For production deployments with caching, use the Varnish-enabled configuration:

```sh
# Use the Varnish-enabled docker-compose configuration
docker compose -f contrib/docker-compose-varnish.yml up
```

Every compose file runs three containers: a one-shot import (full image), the API on the serve-only
image with no import tooling and no admin credentials, and an optional updater:

```sh
docker compose -f contrib/docker-compose.yml --profile updates up -d
```

Or see [EXTERNAL-POSTGIS.md](docs/EXTERNAL-POSTGIS.md) for complete setup instructions with custom configurations.

After the import is complete, you can access the Nominatim API at `http://localhost:8080/search.php?q=avenue%20pasteur` (or `http://localhost/search.php?q=avenue%20pasteur` when using the Varnish configuration).

## Accessing Different Versions

You can pull specific versions of the Nominatim Docker image by specifying the tag, e.g.:

```sh
docker pull ghcr.io/maxysoft/nominatim-docker:v5.3.2-1bc9f5b
```

For a list of available tags, please refer to the [Docker Hub page](https://hub.docker.com/r/maxysoft/nominatim-docker/tags) or [Github Packages](https://github.com/maxysoft/nominatim-docker/pkgs/container/nominatim-docker).

## Security Information

For information regarding the latest supported security version and security policies for Nominatim, please refer to the official Nominatim security documentation: [Nominatim Security Policy](https://github.com/osm-search/Nominatim/blob/master/SECURITY.md).

## Detailed Usage and Configuration

For comprehensive instructions on advanced configuration, importing custom PBF files, persistent data, updating the database, PostgreSQL tuning, and more, please refer to the [detailed how-to guide](howto.md).

## Project goals and alternatives

This project has been modified to provide better separation of concerns by using an external PostgreSQL/PostGIS database instead of running PostgreSQL inside the Nominatim container. This approach offers several advantages:

- Better resource management and scalability
- Easier database maintenance and backups  
- Ability to use managed database services (AWS RDS, Google Cloud SQL, etc.)
- Simplified container deployment and updates

The trade-off is slightly more complex setup, but with better operational characteristics for production use.

If you're looking for other projects with different architectures, check out <https://github.com/smithmicro/n7m>.

## Automated Builds

Docker images are automatically built and pushed to both Docker Hub and GitHub Container Registry on every merge to the master branch. Images are tagged with:

**GitHub Container Registry (always available):**

- `ghcr.io/maxysoft/nominatim-docker:v<version>-<commit-sha>` - Specific version and commit (e.g., `v5.3.2-84b3d22`)
- `ghcr.io/maxysoft/nominatim-docker:latest` - Always points to the latest master build
- `ghcr.io/maxysoft/nominatim-docker:serve` and `:v<version>-<commit-sha>-serve` - Slim serve-only variant (no import tooling; see [DEPLOYMENT.md](docs/DEPLOYMENT.md))

**Docker Hub (when secrets are configured):**

- `maxysoft/nominatim-docker:v<version>-<commit-sha>` - Specific version and commit (e.g., `v5.3.2-84b3d22`)
- `maxysoft/nominatim-docker:latest` - Always points to the latest master build
- `maxysoft/nominatim-docker:serve` and `:v<version>-<commit-sha>-serve` - Slim serve-only variant

This ensures every change is automatically available as a Docker image for testing and deployment, with GitHub Container Registry as the primary fallback when Docker Hub credentials are not available.

## Contributors

This project is a fork of [mediagis/nominatim-docker](https://github.com/mediagis/nominatim-docker);
the many people who built the original are credited in the upstream
[contributors list](https://github.com/mediagis/nominatim-docker#contributors-).
Contributions of any kind welcome!
