# syntax=docker/dockerfile:1.7
ARG NOMINATIM_VERSION=5.3.2
ARG USER_AGENT=maxysoft/nominatim-docker:${NOMINATIM_VERSION}

# Pinned by digest so a mutated tag can never change the base image.
# To upgrade: docker pull debian:13.4-slim, read the new digest, update here.
ARG BASE_IMAGE=debian:13.4-slim@sha256:cedb1ef40439206b673ee8b33a46a03a0c9fa90bf3732f54704f99cb061d2c5a
ARG GO_IMAGE=golang:1.24-bookworm

# Fixed IDs so a rebuilt image keeps working with existing data volumes.
ARG NOMINATIM_UID=1000
ARG NOMINATIM_GID=1000


# ---------------------------------------------------------------------------
# Stage 1 — the entrypoint binary.
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
# Stage 2 — the Python environment.
#
# Built into a virtualenv rather than installed over the system interpreter with
# --break-system-packages, so pip never overwrites dpkg-owned files and the
# whole runtime is one directory to copy.
#
# --system-site-packages so PyICU is taken from Debian's prebuilt python3-icu.
# PyICU ships no wheel, so pip would compile a C++ extension — and on the arm64
# publish leg that happens under QEMU. Every other dependency is a wheel, which
# is why no compiler is installed here at all.
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

COPY requirements.txt /tmp/requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip,sharing=locked \
    python3 -m venv --system-site-packages /opt/nominatim \
    && /opt/nominatim/bin/pip install --no-cache-dir --upgrade pip setuptools wheel \
    && /opt/nominatim/bin/pip install --no-cache-dir --require-hashes -r /tmp/requirements.txt \
    && find /opt/nominatim -name '__pycache__' -type d -prune -exec rm -rf {} +


# ---------------------------------------------------------------------------
# Stage 3 — runtime.
#
# Deliberately omitted versus the previous image: sudo (a setuid-root binary
# needed only to move privilege downwards, which the entrypoint now does with a
# direct fork+setuid), sshpass and openssh-client (the supplementary datasets
# are fetched over HTTPS instead of scp with host-key checking disabled), curl
# (downloads and the healthcheck are in the entrypoint), and every -dev package.
# ---------------------------------------------------------------------------
FROM ${BASE_IMAGE} AS runtime

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
    && printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d \
    && chmod +x /usr/sbin/policy-rc.d \
    && apt-get -y update -qq \
    && apt-get -y install -o APT::Install-Recommends=false -o APT::Install-Suggests=false \
        ca-certificates \
        osm2pgsql \
        postgresql-client \
        python3 \
        python3-icu \
    && rm -f /usr/sbin/policy-rc.d \
    && rm -rf /var/lib/apt/lists/*

# The account is written directly rather than with useradd, because the passwd
# package that provides it also installs five setuid-root binaries. The previous
# image ran useradd at container start, so the UID was whatever the kernel
# happened to assign — and a data volume written by one build could be
# unreadable to the next.
RUN echo "nominatim:x:${NOMINATIM_UID}:${NOMINATIM_GID}::${NOMINATIM_HOME}:/usr/sbin/nologin" >> /etc/passwd \
    && echo "nominatim:x:${NOMINATIM_GID}:" >> /etc/group \
    && echo "nominatim:!*:20000:0:99999:7:::" >> /etc/shadow \
    && mkdir -p ${NOMINATIM_HOME} ${PROJECT_DIR} \
    && chown ${NOMINATIM_UID}:${NOMINATIM_GID} ${NOMINATIM_HOME} ${PROJECT_DIR}

COPY --from=py-build /opt/nominatim /opt/nominatim
COPY --from=go-build /out/nominatim-ctl /usr/local/bin/nominatim-ctl

# Strip every setuid/setgid bit the base packages ship (passwd, su, mount,
# gpasswd, ...). Nothing here needs them: the entrypoint starts as root and
# lowers privilege with a direct setuid, which requires no setuid binary. What
# remains is a local privilege-escalation surface reachable by the very account
# Gunicorn runs as, so it is removed rather than merely left unused.
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

# Starts as root only to take ownership of the mounted volume; every workload
# process is spawned as the nominatim user. No setuid binary remains in the
# image, so the container can run with no-new-privileges.
ENTRYPOINT ["/usr/local/bin/nominatim-ctl"]
CMD ["serve"]
