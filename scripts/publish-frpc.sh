#!/usr/bin/env bash
# Publish a (already built and, in release CI, Authenticode-signed) frpc binary
# to gs://ironflock-downloads/device-agent/frpc/, with a SHA-256 manifest next
# to it.
#
# The agent refuses an frpc without a valid frpc-download signature for exactly
# <goos>/<goarch>/<FRP_VERSION>/frpc[.exe] (system.verifyRemoteSignature). The
# .sig is added at promotion (scripts/sign-release.sh). Once
# that has happened the object is frozen: unlike the agent, frpc is published
# again by every release under the same FRP_VERSION path, and new bytes under
# the old .sig would make every verifying agent refuse it (and the appliance
# mirror caches the path as immutable). Bump FRP_VERSION, or delete the
# objects by hand, to publish a different frpc.
#
# A local <binary>.sig (a development signature) is uploaded too, but only if
# it verifies for that path.
#
# The agent downloads it from <its source tree>/frpc (system.downloadFrpc):
# device-agent/frpc here; agents that fall back to the frozen legacy tree read
# re-agent/frpc, which holds the same FRP_VERSION.
#
# Usage: scripts/publish-frpc.sh <goos> <goarch> <frpc-binary-path>
set -euo pipefail

goos="${1:?usage: publish-frpc.sh <goos> <goarch> <path>}"
goarch="${2:?usage: publish-frpc.sh <goos> <goarch> <path>}"
binary="${3:?usage: publish-frpc.sh <goos> <goarch> <path>}"

FRP_VERSION="$(grep -oE 'FRP_VERSION = "[^"]+"' src/embedded/frpc.go | cut -d'"' -f2)"

remote_name="frpc"
if [ "$goos" = "windows" ]; then
    remote_name="frpc.exe"
fi

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

dest="gs://ironflock-downloads/device-agent/frpc/${goos}/${goarch}/${FRP_VERSION}/${remote_name}"

if [ ! -e "${binary}.sig" ] && gsutil -q stat "${dest}.sig"; then
    echo "$dest is already signed; keeping it (frozen once signed, see above)"
    exit 0
fi
if [ -e "${binary}.sig" ]; then
    binary_abs="$(cd "$(dirname "$binary")" && pwd)/$(basename "$binary")"
    (cd src && go run ./cmd/sign-update verify -kind frpc -os "$goos" -arch "$goarch" -version "$FRP_VERSION" "$binary_abs")
fi

cp_nocache() {
    gsutil -q -h "Cache-Control:public, max-age=0" cp "$1" "$2"
}

cp_nocache "$binary" "$dest"

sha256_of "$binary" > "${binary}.sha256"
cp_nocache "${binary}.sha256" "${dest}.sha256"
rm -f "${binary}.sha256"

if [ -e "${binary}.sig" ]; then
    cp_nocache "${binary}.sig" "${dest}.sig"
fi

echo "Published $dest (+ .sha256)"
