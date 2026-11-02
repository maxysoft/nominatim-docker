# syntax=docker/dockerfile:1.27.0@sha256:bde3983e9c939224420ddaf6b784cc30e09b035a4dea01f581230c50809f372e
ARG NOMINATIM_VERSION=5.3.2
ARG USER_AGENT=maxysoft/nominatim-docker:${NOMINATIM_VERSION}

ARG NOMINATIM_UID=1000
ARG NOMINATIM_GID=1000


# The Makefile reads this line with sed: keep it on one line ending in AS go-build.
FROM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS go-build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/nominatim-ctl ./cmd/nominatim-ctl


# The Makefile reads this line with sed: keep it on one line ending in AS base.
FROM debian:13.7-slim@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4 AS base

FROM base AS py-build

ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8

# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
    && echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' >/etc/apt/apt.conf.d/keep-cache \
    && apt-get -y update -qq \
    && apt-get -y install -o APT::Install-Recommends=false -o APT::Install-Suggests=false \
        ca-certificates \
        python3-icu \
        python3-venv

COPY requirements.txt /tmp/requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip,sharing=locked \
    python3 -m venv --system-site-packages /opt/nominatim \
    && /opt/nominatim/bin/pip install --disable-pip-version-check --no-compile --require-hashes -r /tmp/requirements.txt \
    && /opt/nominatim/bin/pip uninstall -y --disable-pip-version-check pip


FROM base AS serve-base

ARG NOMINATIM_VERSION
ARG USER_AGENT
ARG NOMINATIM_UID
ARG NOMINATIM_GID
ARG NOMINATIM_HOME=/var/lib/nominatim

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    PYTHONUNBUFFERED=1 \
    PATH=/opt/nominatim/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PROJECT_DIR=/nominatim \
    USER_AGENT=${USER_AGENT}

# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
    && echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' >/etc/apt/apt.conf.d/keep-cache \
    && apt-get -y update -qq \
    && apt-get -y install -o APT::Install-Recommends=false -o APT::Install-Suggests=false \
        ca-certificates \
        python3 \
        python3-icu

RUN echo "nominatim:x:${NOMINATIM_UID}:${NOMINATIM_GID}::${NOMINATIM_HOME}:/usr/sbin/nologin" >> /etc/passwd \
    && echo "nominatim:x:${NOMINATIM_GID}:" >> /etc/group \
    && echo "nominatim:!*:20000:0:99999:7:::" >> /etc/shadow \
    && mkdir -p ${NOMINATIM_HOME} ${PROJECT_DIR} \
    && chown ${NOMINATIM_UID}:${NOMINATIM_GID} ${NOMINATIM_HOME} ${PROJECT_DIR}

COPY --from=py-build /opt/nominatim /opt/nominatim

RUN find / -xdev -type f -perm /6000 -exec chmod ug-s {} + \
    && [ -z "$(find / -xdev -type f -perm /6000)" ]

WORKDIR ${PROJECT_DIR}
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=10s --start-period=48h --retries=3 \
    CMD ["/usr/local/bin/nominatim-ctl", "healthcheck"]

LABEL org.opencontainers.image.title="nominatim-docker" \
      org.opencontainers.image.description="Nominatim geocoder against an external PostgreSQL/PostGIS server" \
      org.opencontainers.image.version="${NOMINATIM_VERSION}" \
      org.opencontainers.image.source="https://github.com/maxysoft/nominatim-docker" \
      org.opencontainers.image.licenses="GPL-2.0-or-later"

ENTRYPOINT ["/usr/local/bin/nominatim-ctl"]
CMD ["serve"]


FROM serve-base AS serve

COPY --from=go-build /out/nominatim-ctl /usr/local/bin/nominatim-ctl


FROM serve-base AS full

# hadolint ignore=DL3008
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get -y update -qq \
    && apt-get -y install -o APT::Install-Recommends=false -o APT::Install-Suggests=false \
        osm2pgsql \
    # psql looks unused, but nominatim import pipes SQL into a psql subprocess: removing it breaks every import.
        postgresql-client \
    && rm -f /usr/bin/osm2pgsql-gen \
    && dpkg --purge --force-depends \
        libopencv-imgcodecs410 libopencv-imgproc410 libopencv-core410 \
        libgdal36 mesa-libgallium libllvm19 libz3-4 \
        libgbm1 libglx-mesa0 libglx0 \
        libgdcm3.0t64 libnetcdf22 libhdf5-310 libhdf5-hl-310 \
        libpoppler147 libcfitsio10t64 libxerces-c3.2t64 \
        libspatialite8t64 libgeotiff5 \
    && osm2pgsql --version \
    && find / -xdev -type f -perm /6000 -exec chmod ug-s {} + \
    && [ -z "$(find / -xdev -type f -perm /6000)" ]

COPY --from=go-build /out/nominatim-ctl /usr/local/bin/nominatim-ctl
