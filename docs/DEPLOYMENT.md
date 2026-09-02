# Deployment Setup

## Automatic Docker Builds

This repository includes a GitHub Actions workflow that automatically builds and publishes Docker images on every push to the master branch. Images are pushed to both Docker Hub and GitHub Container Registry.

### Setup Requirements

The workflow automatically pushes to **GitHub Container Registry** using the built-in `GITHUB_TOKEN` with `packages: write` permission (no additional configuration needed).

To also enable **Docker Hub** publishing, the repository maintainer can optionally configure these secrets:

1. **DOCKERHUB_USERNAME** - Docker Hub username for the `maxysoft` organization
2. **DOCKERHUB_TOKEN** - Docker Hub access token with push permissions

If Docker Hub secrets are not configured, images will only be pushed to GitHub Container Registry.

### Image variants

- **Full** (`latest`, `v<version>-<sha>`) is the default: import, replication and serving.
- **Serve** (`serve`, `v<version>-<sha>-serve`) does API serving only. osm2pgsql and
  postgresql-client are not installed, so the image is smaller and the long-running exposed
  container has less attack surface. It refuses to run an import and rejects `UPDATE_MODE`:
  run the import (and any replication) with the full image against the same database, then
  point the serving container at this tag. Built locally with `docker build --target serve .`.

Every `contrib/docker-compose*.yml` combines the two: a one-shot `nominatim-ctl import` container on
the full image provisions the database, imports and exits; the API runs on the serve image with no
admin credentials and no import tooling; an optional `nominatim-ctl replicate` container on the full
image (`--profile updates`) applies replication diffs. Re-running the import container is safe: it
finds the completed import, reconciles the role passwords (so a rotated `NOMINATIM_PASSWORD` takes
effect) and exits. To import a newer extract into the same database run
`docker compose -f contrib/docker-compose.yml run --rm nominatim-import reimport`, the one-shot form
of "drop and import again". The import and updater containers share the project volume
(the updater needs the flatnode file when flatnode storage is in use) and therefore must agree on
every setting that lands in `.env`; the example keeps them in one shared block. The API has its own
volume.

All shipped compose files run the container with `read_only: true`; the runtime writes only to
`/nominatim` (volume), `/tmp` and `$HOME` (tmpfs), and `/dev/shm`.

### Generated Tags

The workflow creates tags on both registries when Docker Hub is configured:

**GitHub Container Registry (always available):**
- `ghcr.io/maxysoft/nominatim-docker:v<version>-<commit-sha>` - Specific version and commit (e.g., `v5.1.0-84b3d22`)
- `ghcr.io/maxysoft/nominatim-docker:latest` - Always points to the latest master build
- `ghcr.io/maxysoft/nominatim-docker:v<version>-<commit-sha>-serve` and `:serve` - The serve-only variant

**Docker Hub (when secrets are configured):**
- `maxysoft/nominatim-docker:v<version>-<commit-sha>` - Specific version and commit (e.g., `v5.1.0-84b3d22`)
- `maxysoft/nominatim-docker:latest` - Always points to the latest master build
- `maxysoft/nominatim-docker:v<version>-<commit-sha>-serve` and `:serve` - The serve-only variant

### Build Process

1. Triggered on push to master branch
2. Extracts commit short SHA (first 7 characters)
3. Builds Docker image for `linux/amd64` and `linux/arm64` platforms
4. Pushes to GitHub Container Registry (always)
5. Pushes to Docker Hub with appropriate tags (when secrets are configured)
6. Includes OCI labels for traceability

### Manual Setup Instructions

1. Go to repository Settings → Secrets and variables → Actions
2. Add the required secrets:
   - `DOCKERHUB_USERNAME`: Your Docker Hub username
   - `DOCKERHUB_TOKEN`: Generate from Docker Hub → Account Settings → Security → Access Tokens

The workflow file is located at `.github/workflows/ci.yml` (publish job).