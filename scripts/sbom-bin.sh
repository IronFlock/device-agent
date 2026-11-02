#!/usr/bin/env bash
# The CycloneDX SBOM of one built binary, <binary>.cdx.json, from the build
# info Go writes into it (cyclonedx-gomod bin): exactly the modules linked in.
# release.yml attests it; publish.sh and publish-frpc.sh publish it next to the
# binary.
#
#   scripts/sbom-bin.sh <binary>
#
# An agent binary (main module reagent) gets the release version
# (src/release/version.txt) as its version, and every agent but the Windows one
# lists the frpc it embeds (scripts/build.sh) as a component with that frpc's
# own modules. That frpc is built again here for the binary's platform, from
# the same frp/ module with the same toolchain, so its build info is the
# embedded one's. Its references carry the prefix "frpc:", because some modules
# occur in both.
#
# cyclonedx-gomod writes the platform it gets from `go env` into the main
# component's purl, so it runs with the binary's GOOS, GOARCH and GOARM: an arm
# SBOM made on an amd64 runner says arm.
set -euo pipefail
cd "$(dirname "$0")/.."

binary="${1:?usage: scripts/sbom-bin.sh <binary>}"
CYCLONEDX_GOMOD_VERSION="$(grep -oE '^CYCLONEDX_GOMOD_VERSION := "[^"]+"' Justfile | cut -d'"' -f2)"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
GOBIN="$work/bin" go install "github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@${CYCLONEDX_GOMOD_VERSION}"

info=$(go version -m "$binary")
# setting <key>: a build setting of the binary (GOOS, GOARCH, GOARM).
setting() {
    awk -F'\t' -v k="$1" '$2 == "build" && index($3, k "=") == 1 { print substr($3, length(k) + 2) }' <<< "$info"
}
platform=(GOOS="$(setting GOOS)" GOARCH="$(setting GOARCH)")
goarm=$(setting GOARM)
[ -z "$goarm" ] || platform+=(GOARM="$goarm")
main_module=$(awk -F'\t' '$2 == "mod" { print $3 }' <<< "$info")

# sbom <binary> <output> [cyclonedx-gomod flags...]
sbom() {
    local bin="$1" out="$2"
    shift 2
    env "${platform[@]}" "$work/bin/cyclonedx-gomod" bin -json "$@" -output "$out" "$bin"
}

if [ "$main_module" != reagent ]; then
    sbom "$binary" "$binary.cdx.json"
    exit 0
fi

sbom "$binary" "$work/agent.cdx.json" -version "v$(tr -d '[:space:]' < src/release/version.txt)"
if [ "$(setting GOOS)" = windows ]; then
    mv "$work/agent.cdx.json" "$binary.cdx.json"
    exit 0
fi

(
    [ -z "$goarm" ] || export GOARM="$goarm"
    scripts/build-frpc.sh "$(setting GOOS)" "$(setting GOARCH)" "$work/frpc" >&2
)
sbom "$work/frpc" "$work/frpc.cdx.json"
jq --slurpfile frpc "$work/frpc.cdx.json" '
    def p: "frpc:" + .;
    $frpc[0] as $f
    | .metadata.component["bom-ref"] as $main
    | ($f.metadata.component["bom-ref"] | p) as $root
    | .components += [
        $f.metadata.component
        | .["bom-ref"] = $root
        | .description = "Embedded in the agent binary, which runs it for remote-access tunnels"
        | .components = [$f.components[]? | .["bom-ref"] |= p]
    ]
    | if any(.dependencies[]?; .ref == $main) then . else .dependencies += [{ref: $main}] end
    | .dependencies = [.dependencies[] | if .ref == $main then .dependsOn = ((.dependsOn // []) + [$root]) else . end]
        + [$f.dependencies[]? | .ref |= p | .dependsOn = ((.dependsOn // []) | map(p))]
' "$work/agent.cdx.json" > "$binary.cdx.json"
