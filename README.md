# Nominatim Docker

100% working container for [Nominatim](https://github.com/openstreetmap/Nominatim).

![Nominatim Version](https://img.shields.io/badge/Nominatim%20Version-5.3.2-blue?style=flat-square) ![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/maxysoft/nominatim-docker/ci.yml?branch=master&style=flat-square) ![Docker Pulls](https://img.shields.io/docker/pulls/maxysoft/nominatim-docker?style=flat-square) ![Docker Image Size with architecture (latest by date/latest semver)](https://img.shields.io/docker/image-size/maxysoft/nominatim-docker?style=flat-square)

> [!IMPORTANT]  
> ⚠️ The following code modifications and implementations were generated with the assistance of **AI (Claude)**.  
> Please review carefully before using in production.

> **⚠️ Important:** This version requires an external PostgreSQL database with PostGIS. See [EXTERNAL-POSTGIS.md](docs/EXTERNAL-POSTGIS.md) for setup instructions.

## Quick Start

The easiest way to use Nominatim Docker is by pulling the pre-built images from [Docker Hub](https://hub.docker.com/r/maxysoft/nominatim-docker) or [Github Packages](https://github.com/maxysoft/nominatim-docker/pkgs/container/nominatim-docker).

To quickly get a Nominatim instance up and running with a small dataset (e.g., Monaco):

```sh
# Set the three database passwords first; every compose file refuses to start without them
cp contrib/.env.example contrib/.env
$EDITOR contrib/.env

# Use the provided docker-compose configuration
docker compose -f contrib/docker-compose.yml up
```

For production deployments with caching, use the Varnish-enabled configuration:

```sh
# Use the Varnish-enabled docker-compose configuration
docker compose -f contrib/docker-compose-varnish.yml up
```

Every compose file runs a one-shot import, the API and an optional updater (see
[howto.md](howto.md#docker-compose)); start the updater with:

```sh
docker compose -f contrib/docker-compose.yml --profile updates up -d
```

Or see [EXTERNAL-POSTGIS.md](docs/EXTERNAL-POSTGIS.md) for complete setup instructions with custom configurations.

After the import is complete, you can access the Nominatim API at `http://localhost:8080/search.php?q=avenue%20pasteur` (or `http://localhost/search.php?q=avenue%20pasteur` when using the Varnish configuration).

## Images

Images are published to `ghcr.io/maxysoft/nominatim-docker` (and Docker Hub as
`maxysoft/nominatim-docker`), tagged `latest`, `serve` and `v<version>-<sha>[-serve]`; see
[DEPLOYMENT.md](docs/DEPLOYMENT.md).

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

## Contributors

This project is a fork of [mediagis/nominatim-docker](https://github.com/mediagis/nominatim-docker);
the many people who built the original are credited in the upstream
[contributors list](https://github.com/mediagis/nominatim-docker#contributors-).
Contributions of any kind welcome!
