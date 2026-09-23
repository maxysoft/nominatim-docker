# Deployment Setup

## Automatic Docker Builds

This repository includes a GitHub Actions workflow that automatically builds and publishes Docker images on every push to the master branch, once the static, test and integration jobs pass. Images are pushed to both Docker Hub and GitHub Container Registry.

### Setup Requirements

The workflow automatically pushes to **GitHub Container Registry** using the built-in `GITHUB_TOKEN` with `packages: write` permission (no additional configuration needed).

**Docker Hub** publishing runs only when the repository owner is `maxysoft`, and there it is
mandatory: configure these secrets under Settings → Secrets and variables → Actions:

1. **DOCKERHUB_USERNAME** - Docker Hub username for the `maxysoft` organization
2. **DOCKERHUB_TOKEN** - Docker Hub access token with push permissions (Docker Hub → Account Settings → Security → Access Tokens)

Without them the Docker Hub login fails and the job stops before the GHCR pushes, so nothing is
published. Forks never push to Docker Hub, even with the secrets set; they publish to GHCR only.

### Image variants

- **Full** (`latest`, `v<version>-<sha>`) is the default: import, replication and serving.
- **Serve** (`serve`, `v<version>-<sha>-serve`) does API serving only. osm2pgsql and
  postgresql-client are not installed, so the image is smaller and the long-running exposed
  container has less attack surface. It refuses to run an import and rejects `UPDATE_MODE`:
  run the import (and any replication) with the full image against the same database, then
  point the serving container at this tag. Built locally with `docker build --target serve .`.

Every `contrib/docker-compose*.yml` combines the two; see [howto.md](../howto.md#docker-compose).

All shipped compose files run the container with `read_only: true`; the runtime writes only to
`/nominatim` (volume), `/tmp` and `$HOME` (tmpfs), and `/dev/shm`.

### Generated Tags

The workflow creates tags on both registries when Docker Hub is configured. Pushes to the `test`
branch publish the same tags with a `-test` suffix for pre-merge validation.

**GitHub Container Registry (always available):**
- `ghcr.io/maxysoft/nominatim-docker:v<version>-<commit-sha>` - Specific version and commit (e.g., `v5.3.2-84b3d22`)
- `ghcr.io/maxysoft/nominatim-docker:latest` - Always points to the latest master build
- `ghcr.io/maxysoft/nominatim-docker:v<version>-<commit-sha>-serve` and `:serve` - The serve-only variant

**Docker Hub (when secrets are configured):**
- `maxysoft/nominatim-docker:v<version>-<commit-sha>` - Specific version and commit (e.g., `v5.3.2-84b3d22`)
- `maxysoft/nominatim-docker:latest` - Always points to the latest master build
- `maxysoft/nominatim-docker:v<version>-<commit-sha>-serve` and `:serve` - The serve-only variant

The workflow file is located at `.github/workflows/ci.yml` (publish job).