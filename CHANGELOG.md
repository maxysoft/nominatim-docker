# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Releases

### v5.3.2 — 2026-04-22

- **Changed:** Merge from upstream (mediagis/nominatim-docker) to sync docs and contributors
- **Changed:** Bump nominatim version to 5.3.2
- **Changed:** Move postgres config in a separate file and mount it in docker compose
- **Fixed:** Corrected `contrib/docker-compose-local.yml` bind mount path (was resolving to contrib/contrib/...), which caused the PostgreSQL container to fail on startup
- **Fixed:** Various markdown files syntax issues
- **Added:** Different postgres configs
- **Added:** New CI helper script `.github/workflows/assert-json-field` to assert specific JSON response fields (dot-path) against regexes with retries
- **Test:** Enhanced CI "API endpoints coverage" scenario to validate status fields, search result fields (name/class/place_rank), addressdetails, polygon GeoJSON, reverse lookup, lookup/details by osmtype+osmid, and Content-Type header

### v5.3.0 — 2026-04-04

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
