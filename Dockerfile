ARG NOMINATIM_VERSION=5.3.2
ARG USER_AGENT=maxysoft/nominatim-docker:${NOMINATIM_VERSION}
# Pin to a specific digest so tag mutations can never change the base image unexpectedly.
# To upgrade: docker pull debian:13.4-slim, get new digest, update ARG below.
ARG BASE_IMAGE=debian:13.4-slim@sha256:cedb1ef40439206b673ee8b33a46a03a0c9fa90bf3732f54704f99cb061d2c5a

FROM ${BASE_IMAGE} AS build

ENV DEBIAN_FRONTEND=noninteractive
ENV LANG=C.UTF-8

WORKDIR /app

# Inspired by https://github.com/reproducible-containers/buildkit-cache-dance?tab=readme-ov-file#apt-get-github-actions
RUN  \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    # Keep downloaded APT packages in the docker build cache
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' >/etc/apt/apt.conf.d/keep-cache && \
    # Do not start daemons after installation.
    echo '#!/bin/sh\nexit 101' > /usr/sbin/policy-rc.d \
    && chmod +x /usr/sbin/policy-rc.d \
    # Install all required packages.
    && apt-get -y update -qq \
    && apt-get -y install \
        locales \
        ca-certificates \
    && locale-gen en_US.UTF-8 \
    && echo 'LANG=en_US.UTF-8' > /etc/default/locale \
    && apt-get -y install \
        -o APT::Install-Recommends="false" \
        -o APT::Install-Suggests="false" \
        # Build tools from sources. \
        build-essential \
        osm2pgsql \
        pkg-config \
        libicu-dev \
        python3-dev \
        python3-pip \
        python3-icu \
        # PostgreSQL client tools for external database connection. \
        postgresql-client \
        # Misc.
        curl \
        sudo \
        sshpass \
        openssh-client




ARG NOMINATIM_VERSION
ARG USER_AGENT

# Nominatim install.
RUN --mount=type=cache,target=/root/.cache/pip,sharing=locked pip install --break-system-packages \
    nominatim-db==$NOMINATIM_VERSION \
    osmium \
    psycopg[binary] \
    falcon \
    "gunicorn>=25.0" \
    nominatim-api


# remove build-only packages
RUN true \
    # Remove development and unused packages.
    && apt-get -y remove --purge --auto-remove \
        build-essential \
    # Clear temporary files and directories.
    && rm -rf \
        /tmp/* \
        /var/tmp/* \
    && pip cache purge



COPY config.sh /app/config.sh
COPY init.sh /app/init.sh
COPY start.sh /app/start.sh

# Collapse image to single layer.
FROM scratch

COPY --from=build / /

# Please override this
ENV NOMINATIM_PASSWORD=qaIACxO6wMR3
ENV WARMUP_ON_STARTUP=false

ENV PROJECT_DIR=/nominatim

ARG USER_AGENT
ENV USER_AGENT=${USER_AGENT}

WORKDIR /app

EXPOSE 8080

COPY conf.d/env $PROJECT_DIR/.env

CMD ["/app/start.sh"]
