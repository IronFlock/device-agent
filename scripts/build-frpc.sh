#!/usr/bin/env bash
# Build frpc from source for a target platform, from the frp/ module.
#
# frp/go.mod pins the frp version (= FRP_VERSION in src/embedded/frpc.go, see
# scripts/check-frp-version.sh) and raises golang.org/x/net and x/crypto above
# frp's own requirements; go.sum and the Go checksum database verify every
# byte. We build frpc ourselves rather than shipping fatedier/frp release
# binaries so that (a) we control the supply chain, (b) security fixes in its
# dependencies do not wait for an frp release, (c) we can Authenticode-sign
# the Windows binary, and (d) the hash differs from the widely-flagged official
# binary. Signing does NOT clear frp's intentional PUA/HackTool antivirus
# classification — that is handled operationally on-device (Defender
# exclusion / WDAC signer rule).
#
# -tags noweb skips the embedded web dashboard (its assets are not part of the
# module download); the agent drives frpc through its admin HTTP API only.
# -s -w strips symbols like the upstream release binaries: frpc is embedded in
# every agent download, so size matters on devices.
#
# Usage: scripts/build-frpc.sh <goos> <goarch> <output-path>   (GOARM from the environment for arm)
set -euo pipefail

target_os="${1:?usage: build-frpc.sh <goos> <goarch> <output-path>}"
target_arch="${2:?usage: build-frpc.sh <goos> <goarch> <output-path>}"
out="${3:?usage: build-frpc.sh <goos> <goarch> <output-path>}"

root="$(cd "$(dirname "$0")/.." && pwd)"
FRP_VERSION="$(grep -oE 'FRP_VERSION = "[^"]+"' "$root/src/embedded/frpc.go" | cut -d'"' -f2)"
echo "Building frpc v${FRP_VERSION} for ${target_os}/${target_arch}${GOARM:+/v$GOARM} from frp/"

mkdir -p "$(dirname "$out")"
out_abs="$(cd "$(dirname "$out")" && pwd)/$(basename "$out")"
(
    cd "$root/frp"
    GOOS="$target_os" GOARCH="$target_arch" CGO_ENABLED=0 \
        go build -tags noweb -trimpath -ldflags "-s -w" -o "$out_abs" github.com/fatedier/frp/cmd/frpc
)

echo "Built frpc -> $out_abs ($(wc -c < "$out_abs") bytes)"
