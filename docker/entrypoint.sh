#!/bin/bash
# Entrypoint for the IronFlock Device Management Agent docker image.
#
# Generates device-config.flock from AGENT_* env vars ONLY if it does
# not already exist (the config file is typically on a persistent
# volume, so this only runs on first boot). Defaults match the base
# seed created by REaccounting/database/install/create_init_data.sql:
# admin account, default swarm, fixed ironflock-instance device
# identity. Override the AGENT_* env vars to connect the agent to a
# different device/swarm (e.g. the dev "device-a" in "demo_swarm").
#
# AGENT_SECRET has NO default and must be set. The seed generates the
# device secret per install (an appliance takes it from
# APPLIANCE_DEVICE_SECRET in its .env); the static default this file
# used to carry was the same on every install.

set -e

CONFIG_PATH="${AGENT_CONFIG_PATH:-/app/device-config.flock}"

if [ -f "${CONFIG_PATH}" ]; then
    echo "Reusing existing device config at ${CONFIG_PATH}."
else
    # ------------------------------------------------------------------
    # Agent config defaults (must stay in sync with create_init_data.sql).
    # The secret is the plaintext secret the agent presents to the router
    # for WAMP-CRA; it is stored raw in device.t_device.secret. Required,
    # no default (see the header).
    # ------------------------------------------------------------------
    AGENT_DEVICE_NAME="${AGENT_DEVICE_NAME:-ironflock-instance}"
    AGENT_DEVICE_KEY="${AGENT_DEVICE_KEY:-1}"
    AGENT_SWARM_KEY="${AGENT_SWARM_KEY:-1}"
    AGENT_SWARM_NAME="${AGENT_SWARM_NAME:-default}"
    AGENT_SWARM_OWNER="${AGENT_SWARM_OWNER:-admin}"
    AGENT_SECRET="${AGENT_SECRET:?AGENT_SECRET is required: the secret stored for this device in device.t_device.secret}"
    AGENT_SERIAL_NUMBER="${AGENT_SERIAL_NUMBER:-00000000-0000-0000-0000-000000000001}"
    AGENT_DEVICE_ENDPOINT_URL="${AGENT_DEVICE_ENDPOINT_URL:-ws://ironflock_router:8080/ws-re-dev}"
    AGENT_DOCKER_REGISTRY_URL="${AGENT_DOCKER_REGISTRY_URL:-app-registry:5000/}"
    AGENT_DOCKER_MAIN_REPOSITORY="${AGENT_DOCKER_MAIN_REPOSITORY:-apps/}"

    # Wifi (password / wlanssid) is intentionally left unset — the
    # appliance agent runs inside Docker with host networking and has
    # no wifi to manage.
    echo "Generating device config at ${CONFIG_PATH}..."
    cat > "${CONFIG_PATH}" <<EOF
{
  "name": "${AGENT_DEVICE_NAME}",
  "secret": "${AGENT_SECRET}",
  "board": {
    "cpu": "",
    "docs": null,
    "board": "",
    "model": "appliance",
    "boardname": "",
    "modelname": "IronFlock Appliance",
    "reflasher": false,
    "architecture": ""
  },
  "status": "DISCONNECTED",
  "swarm_key": ${AGENT_SWARM_KEY},
  "device_key": ${AGENT_DEVICE_KEY},
  "swarm_name": "${AGENT_SWARM_NAME}",
  "serial_number": "${AGENT_SERIAL_NUMBER}",
  "authentication": {
    "key": "",
    "certificate": ""
  },
  "swarm_owner_name": "${AGENT_SWARM_OWNER}",
  "device_endpoint_url": "${AGENT_DEVICE_ENDPOINT_URL}",
  "docker_registry_url": "${AGENT_DOCKER_REGISTRY_URL}",
  "docker_main_repository": "${AGENT_DOCKER_MAIN_REPOSITORY}",
  "insecure-registries": "${AGENT_DOCKER_REGISTRY_URL}"
}
EOF
fi

# ------------------------------------------------------------------
# Start dbus (required by the agent for some system bus operations).
# Non-fatal: not every host grants the permissions we need and the
# agent copes without dbus in most local scenarios.
# ------------------------------------------------------------------
dbus-uuidgen > /var/lib/dbus/machine-id 2>/dev/null || true
mkdir -p /var/run/dbus
dbus-daemon --config-file=/usr/share/dbus-1/system.conf --print-address 2>/dev/null || true

AGENT_ENV="${AGENT_ENV:-local}"
AGENT_NMW="${AGENT_NMW:-false}"

echo "Starting device-agent for device '${AGENT_DEVICE_NAME}' (swarm=${AGENT_SWARM_KEY}, device=${AGENT_DEVICE_KEY}, env=${AGENT_ENV})..."
exec device-agent -config "${CONFIG_PATH}" -prettyLogging -env="${AGENT_ENV}" -nmw="${AGENT_NMW}" "$@"
