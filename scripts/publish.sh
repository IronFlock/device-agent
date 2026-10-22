#!/bin/bash
# Stage the built agent binaries (build/device-agent-<os>-<arch>) at
# gs://ironflock-downloads/device-agent/<os>/<arch>/<version>/device-agent[.exe],
# each with its .sha256 manifest: the agent verifies OTA downloads against it
# (system.verifyRemoteChecksum) and the Windows service refuses to activate an
# update that does not match.
#
# Staged binaries carry no .sig: agents refuse them until promotion signs them
# (scripts/sign-release.sh). A local build/<binary>.sig
# (a development signature) is uploaded too, but only if it verifies, against
# this tree's AGENT_UPDATE_PUBLIC_KEYS, for the path it goes to.
#
# A version that is already signed in the bucket has been promoted; restaging
# it would put new bytes under its live .sig, so that is refused.
#
# The legacy tree gs://re-agent (binaries named reagent) is frozen at 1.3.0,
# the version that moves devices to the new layout; see scripts/freeze-legacy.sh.
set -euo pipefail

VERSION=$(cat src/release/version.txt)
BUCKET="gs://ironflock-downloads"
TREE="device-agent"

# Portable SHA-256 (ubuntu CI has sha256sum, macOS has shasum).
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# Agent binaries only: build/ also holds frpc-windows-amd64 in release CI
# (publish-frpc.sh publishes that), and sidecars or SBOMs locally.
binaries=()
for path in build/device-agent-*; do
    name=$(basename "$path")
    [[ "$name" =~ ^device-agent-[a-z]+-[a-z0-9]+$ ]] || continue
    binaries+=("$name")
done
if [ "${#binaries[@]}" -eq 0 ]; then
    echo "no agent binaries (build/device-agent-<os>-<arch>) to publish" >&2
    exit 1
fi

remote_path() {
    local platform os arch binary_name="device-agent"
    platform=${1#device-agent-}
    os=${platform%%-*}
    arch=${platform#*-}
    if [ "$os" == "windows" ]; then
        binary_name="device-agent.exe"
    fi
    echo "${BUCKET}/${TREE}/${os}/${arch}/${VERSION}/${binary_name}"
}

signed=()
for element in "${binaries[@]}"; do
    if [ -e "build/$element.sig" ]; then
        signed+=("../build/$element")
    elif gsutil -q stat "$(remote_path "$element").sig"; then
        echo "$(remote_path "$element") is already signed (promoted); bump the version instead of restaging it" >&2
        exit 1
    fi
done

# Same <os>/<arch> derivation from the build name as remote_path, same
# VERSION: a signature made for another version or platform fails.
if [ "${#signed[@]}" -gt 0 ]; then
    (cd src && go run ./cmd/sign-update verify -version "$VERSION" "${signed[@]}")
fi

cp_nocache() {
    gsutil -q -h "Cache-Control:public, max-age=0" cp "$1" "$2"
}

for element in "${binaries[@]}"; do
    GCLOUD=$(remote_path "$element")
    cp_nocache "build/$element" "$GCLOUD"

    sha256_of "build/$element" > "build/$element.sha256"
    cp_nocache "build/$element.sha256" "${GCLOUD}.sha256"
    rm -f "build/$element.sha256"

    if [ -e "build/$element.sig" ]; then
        cp_nocache "build/$element.sig" "${GCLOUD}.sig"
    fi
    echo "staged ${GCLOUD}"
done
