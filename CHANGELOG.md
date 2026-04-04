# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2026/04/04]

- Bump nominatim version to 5.3.0 and varnish image to 8.0.1
- Replace ubuntu:24.04 base image with debian:13.4-slim pinned by digest
  via ARG BASE_IMAGE so the base can be overridden at build time
- Add ca-certificates to package list for Debian slim compatibility
- Replace update-locale (incompatible in Debian slim) with direct write
  to /etc/default/locale
- Refactored SCP storage-box credentials in init.sh; use
  STORAGE_USER / STORAGE_HOST / STORAGE_PASSWORD env vars if you need
  to change the default ones
- Fix useradd -p <plaintext-password> in start.sh (password was stored
  unencrypted in /etc/shadow); internal nominatim user needs no login pw
- Merge docker-compose-planet.yml with docker-compose.yml structure
- Replace planet postgres command with planet-optimised settings for
  64 GB RAM / NVMe SSD
- Switch planet compose to bind mounts (/data/db, /data/nominatim)
  and replace mediagis image
- Fix POSTGRES_HOST typo in docker-compose.yml (postgres → nominatim-postgres)
- Default docker-compose.yml now use postgis-18 image

## [2025/10/13]

### Changed
- Documentation update

### Added
- Added missing in import std; in varnish.vcl
- Added missing settings in docker-compose-external-db-varnish.yml
- Added a check that verify if the REPLICATION_URL is reachable, if set. If the REPLICATION_URL is set but unreachable for 
  some reason, nominatim will crash. The check will set REPLICATION_URL to "" (empty) if doesn't work


## [2025/10/11]

### Changed
- Docker image tags now include Nominatim version: `v<version>-<commit-sha>` (e.g., `v5.1.0-291dcde`)
- Updated documentation to reflect new tag format in README.md and DEPLOYMENT.md

### Added
- Changelog to track changes included in each release

## Historical Changes

Previous changes were not tracked in a changelog. For historical information, please refer to the git commit history.
