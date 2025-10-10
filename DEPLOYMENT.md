# Deployment Setup

## Automatic Docker Builds

This repository includes a GitHub Actions workflow that automatically builds and publishes Docker images on every push to the master branch. Images are pushed to both Docker Hub and GitHub Container Registry.

### Setup Requirements

The workflow automatically pushes to **GitHub Container Registry** using the built-in `GITHUB_TOKEN` (no additional configuration needed).

To also enable **Docker Hub** publishing, the repository maintainer can optionally configure these secrets:

1. **DOCKERHUB_USERNAME** - Docker Hub username for the `maxysoft` organization
2. **DOCKERHUB_TOKEN** - Docker Hub access token with push permissions

If Docker Hub secrets are not configured, images will only be pushed to GitHub Container Registry.

### Generated Tags

The workflow creates tags on both registries when Docker Hub is configured:

**GitHub Container Registry (always available):**
- `ghcr.io/maxysoft/nominatim-docker:<commit-sha>` - Specific commit version (e.g., `84b3d22`)
- `ghcr.io/maxysoft/nominatim-docker:latest` - Always points to the latest master build

**Docker Hub (when secrets are configured):**
- `maxysoft/nominatim:<commit-sha>` - Specific commit version (e.g., `84b3d22`)
- `maxysoft/nominatim:latest` - Always points to the latest master build

### Build Process

1. Triggered on push to master branch
2. Extracts commit short SHA (first 7 characters)
3. Builds Docker image for `linux/amd64` and `linux/arm64` platforms
4. Pushes to Docker Hub with appropriate tags
5. Includes OCI labels for traceability

### Manual Setup Instructions

1. Go to repository Settings → Secrets and variables → Actions
2. Add the required secrets:
   - `DOCKERHUB_USERNAME`: Your Docker Hub username
   - `DOCKERHUB_TOKEN`: Generate from Docker Hub → Account Settings → Security → Access Tokens

The workflow file is located at `.github/workflows/auto-build.yml`.