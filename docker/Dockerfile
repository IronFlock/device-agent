# syntax=docker/dockerfile:1
#
# Official IronFlock Device Management Agent runtime image.
#
# This Dockerfile does NOT build the agent. It expects a prebuilt device-agent
# binary (produced by `just build-all-docker`) to be available under
# ./build/device-agent-linux-<arch> at the repo root.
#
# Build context: the repository root (DeviceManagementAgent).
#
# Examples:
#   # build linux/amd64 image using the existing prebuilt binary
#   just build-all-docker                                  # produces build/device-agent-linux-amd64 etc.
#   docker build -f docker/Dockerfile \
#       --build-arg TARGETARCH=amd64 \
#       -t ironflock-agent:local .

FROM ubuntu:24.04

# No default — BuildKit auto-populates TARGETARCH from the target platform
# (host arch if --platform is not set). A default would shadow that and
# pin the apt repo arch (line 37) to amd64 even on arm64 hosts, breaking
# `docker compose build` on M1. Explicit `--build-arg TARGETARCH=...` from
# build-docker-image-{amd64,arm64} still wins.
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive

# `apt-get upgrade` is deliberate: the published ubuntu base image lags the
# archive, so without it the image ships known-vulnerable base packages that a
# plain `install` never touches (it only resolves the named packages). This is a
# dev-stack image, but keeping the base current is cheap. See
# docs/SECURITY-SCANNING.md.
#
# No dbus (nor network-manager, which pulls it in): the agent binds the host's
# /run/dbus into every app when it finds one, and a /run/dbus of this container
# does not exist on the Docker host, so container creation would fail. The
# agent needs D-Bus only for NetworkManager (-nmw, off here) and RAUC.
RUN apt-get update && apt-get -y upgrade && apt-get install -y --no-install-recommends \
        apt-transport-https \
        ca-certificates \
        curl \
        gnupg \
        lsb-release \
        jq \
        libc6 \
        musl \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
        | gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg \
    && echo \
        "deb [arch=${TARGETARCH} signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin \
    && rm -rf /var/lib/apt/lists/*

# The agent picks its paths from what it finds (package layout): a volume at
# the legacy /opt/reagent keeps the legacy paths, so existing installs keep
# their database; otherwise /opt/ironflock/device-agent.
RUN mkdir -p /opt/ironflock/device-agent /app

WORKDIR /app

# Copy the prebuilt binary for the requested arch. Run
# `just build-all-docker` first to produce it. reagent stays as a link for
# compose files that still name it.
COPY build/device-agent-linux-${TARGETARCH} /usr/local/bin/device-agent
RUN chmod +x /usr/local/bin/device-agent && ln -s device-agent /usr/local/bin/reagent

COPY docker/entrypoint.sh /app/entrypoint.sh

RUN chmod +x /app/entrypoint.sh

# Default environment (overridable by docker-compose).
ENV AGENT_ENV=local \
    AGENT_NMW=false \
    AGENT_CONFIG_PATH=/app/device-config.flock

ENTRYPOINT ["/app/entrypoint.sh"]
