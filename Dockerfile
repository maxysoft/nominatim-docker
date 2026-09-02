# syntax=docker/dockerfile:1.7
ARG NOMINATIM_VERSION=5.3.2
ARG USER_AGENT=maxysoft/nominatim-docker:${NOMINATIM_VERSION}

# Pinned by digest so a mutated tag can never change the base image.
# To upgrade: docker buildx imagetools inspect debian:<version>-slim, copy the
# index digest, update here and BASE_IMAGE in the Makefile.
ARG BASE_IMAGE=debian:13.6-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
# Pinned by digest like the base image. To upgrade: docker buildx imagetools
# inspect golang:<version>-bookworm, copy the index digest, update here, in the
# Makefile and in .github/workflows/ci.yml.
ARG GO_IMAGE=golang:1.27.1-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b

# Fixed IDs so a rebuilt image keeps working with existing data volumes.
ARG NOMINATIM_UID=1000
ARG NOMINATIM_GID=1000


# ---------------------------------------------------------------------------
# Stage 1: the entrypoint binary.
# ---------------------------------------------------------------------------
FROM ${GO_IMAGE} AS go-build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/nominatim-ctl ./cmd/nominatim-ctl


# ---------------------------------------------------------------------------
# Stage 2: the Python environment, built as a venv so it copies as one
# directory. --system-site-packages pulls PyICU from Debian's python3-icu: it
# ships no wheel, and compiling it under QEMU on the arm64 leg is why no
# compiler is installed here at all.
# ---------------------------------------------------------------------------
FROM ${BASE_IMAGE} AS py-build

ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8

# hadolint ignore=DL3008  # see docs/REFACTOR.md: base image is digest-pinned; apt floats deliberately
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
    && echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' >/etc/apt/apt.conf.d/keep-cache \
    && apt-get -y update -qq \
    && apt-get -y install -o APT::Install-Recommends=false -o APT::Install-Suggests=false \
        ca-certificates \
        python3-icu \
        python3-venv

# Debian's venv seeds pip from python3-pip-whl. It is not upgraded (that was
# the one unpinned download in the build) and is removed once the pinned,
# hash-checked requirements are installed: the runtime image has no use for a
# package installer.
COPY requirements.txt /tmp/requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip,sharing=locked \
    python3 -m venv --system-site-packages /opt/nominatim \
    && /opt/nominatim/bin/pip install --disable-pip-version-check --require-hashes -r /tmp/requirements.txt \
    && /opt/nominatim/bin/pip uninstall -y --disable-pip-version-check pip \
    && find /opt/nominatim -name '__pycache__' -type d -prune -exec rm -rf {} +


# ---------------------------------------------------------------------------
# Stage 3, serve-base: what the long-running API container needs, nothing
# more. osm2pgsql and postgresql-client live only in the full image; the
# entrypoint refuses to import or replicate without them. The entrypoint
# binary itself is copied in the final stages below, as their last layer, so
# a Go change never re-runs an apt layer.
# ---------------------------------------------------------------------------
FROM ${BASE_IMAGE} AS serve-base

ARG NOMINATIM_VERSION
ARG USER_AGENT
ARG NOMINATIM_UID
ARG NOMINATIM_GID

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    PYTHONUNBUFFERED=1 \
    PATH=/opt/nominatim/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PROJECT_DIR=/nominatim \
    NOMINATIM_HOME=/var/lib/nominatim \
    WARMUP_ON_STARTUP=false \
    USER_AGENT=${USER_AGENT}

# hadolint ignore=DL3008  # see docs/REFACTOR.md: base image is digest-pinned; apt floats deliberately
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
    && echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' >/etc/apt/apt.conf.d/keep-cache \
    && apt-get -y update -qq \
    && apt-get -y install -o APT::Install-Recommends=false -o APT::Install-Suggests=false \
        ca-certificates \
        python3 \
        python3-icu \
    && rm -rf /var/lib/apt/lists/*

# Written directly instead of via useradd, whose package ships five
# setuid-root binaries. Fixed IDs keep data volumes working across rebuilds.
RUN echo "nominatim:x:${NOMINATIM_UID}:${NOMINATIM_GID}::${NOMINATIM_HOME}:/usr/sbin/nologin" >> /etc/passwd \
    && echo "nominatim:x:${NOMINATIM_GID}:" >> /etc/group \
    && echo "nominatim:!*:20000:0:99999:7:::" >> /etc/shadow \
    && mkdir -p ${NOMINATIM_HOME} ${PROJECT_DIR} \
    && chown ${NOMINATIM_UID}:${NOMINATIM_GID} ${NOMINATIM_HOME} ${PROJECT_DIR}

COPY --from=py-build /opt/nominatim /opt/nominatim

# Strip every setuid/setgid bit the base packages ship. The entrypoint drops
# privilege with a direct setuid, so they are only an escalation surface.
RUN find / -xdev -type f -perm /6000 -exec chmod ug-s {} + \
    && [ -z "$(find / -xdev -type f -perm /6000)" ]

WORKDIR ${PROJECT_DIR}
EXPOSE 8080
STOPSIGNAL SIGTERM

# Long start period: a planet import legitimately runs for days before the API
# answers, and must not be reported unhealthy meanwhile.
HEALTHCHECK --interval=30s --timeout=10s --start-period=48h --retries=3 \
    CMD ["/usr/local/bin/nominatim-ctl", "healthcheck"]

LABEL org.opencontainers.image.title="nominatim-docker" \
      org.opencontainers.image.description="Nominatim geocoder against an external PostgreSQL/PostGIS server" \
      org.opencontainers.image.version="${NOMINATIM_VERSION}" \
      org.opencontainers.image.source="https://github.com/maxysoft/nominatim-docker" \
      org.opencontainers.image.licenses="GPL-2.0-or-later"

# Root only to take ownership of the mounted volume; every workload process
# runs as the nominatim user, and no setuid binary remains in the image.
ENTRYPOINT ["/usr/local/bin/nominatim-ctl"]
CMD ["serve"]


# ---------------------------------------------------------------------------
# Stage 4, serve: build with --target serve; published as the -serve tags.
# ---------------------------------------------------------------------------
FROM serve-base AS serve

COPY --from=go-build /out/nominatim-ctl /usr/local/bin/nominatim-ctl


# ---------------------------------------------------------------------------
# Stage 5, full (the default image): serve plus the import and replication
# tooling. `nominatim import` runs osm2pgsql, and `nominatim replication`
# shells out to it for every diff, so both capabilities live here.
# ---------------------------------------------------------------------------
FROM serve-base AS full

# hadolint ignore=DL3008  # see docs/REFACTOR.md: base image is digest-pinned; apt floats deliberately
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d \
    && chmod +x /usr/sbin/policy-rc.d \
    && apt-get -y update -qq \
    && apt-get -y install -o APT::Install-Recommends=false -o APT::Install-Suggests=false \
        osm2pgsql \
    # psql looks unused (this repo talks to PostgreSQL via pgx), but
    # `nominatim import` pipes country_osm_grid.sql.gz and the wikipedia
    # importance dumps into a psql subprocess. Removing it breaks every import.
        postgresql-client \
    # osm2pgsql-gen (never invoked by Nominatim) is what drags in OpenCV ->
    # GDAL -> Mesa -> LLVM: ~195 MB nothing links against. Purging in this
    # same layer is what actually removes the bytes.
    && rm -f /usr/bin/osm2pgsql-gen \
    && dpkg --purge --force-depends \
        libopencv-imgcodecs410 libopencv-imgproc410 libopencv-core410 \
        libgdal36 mesa-libgallium libllvm19 libz3-4 \
        libgbm1 libglx-mesa0 libglx0 \
        libgdcm3.0t64 libnetcdf22 libhdf5-310 libhdf5-hl-310 \
        libpoppler147 libcfitsio10t64 libxerces-c3.2t64 \
        libspatialite8t64 libgeotiff5 \
    # Fail the build, not production, if a future package change makes
    # osm2pgsql actually need any of the above.
    && osm2pgsql --version \
    && rm -f /usr/sbin/policy-rc.d \
    && rm -rf /var/lib/apt/lists/* \
    # The serve stage already stripped and asserted; re-strip and re-assert in
    # case a package installed here ships a setuid/setgid binary.
    && find / -xdev -type f -perm /6000 -exec chmod ug-s {} + \
    && [ -z "$(find / -xdev -type f -perm /6000)" ]

COPY --from=go-build /out/nominatim-ctl /usr/local/bin/nominatim-ctl
