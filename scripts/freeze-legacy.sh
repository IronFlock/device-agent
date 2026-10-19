#!/usr/bin/env bash
# One-off: freeze the legacy tree gs://re-agent at VERSION, the release that
# moves devices to the new layout (1.3.0).
#
#   1. copy VERSION's staged binaries from gs://ironflock-downloads/device-agent/
#      into gs://re-agent under the legacy names (reagent[.exe]), with their
#      .sha256 (same bytes, same digest); not their .sig, which names the
#      file device-agent;
#   2. sign them there (sign-release.sh --legacy), as reagent;
#   3. alias them to latest/;
#   4. point every legacy channel (availableVersions.json, version.txt) at VERSION.
#
# Agents before 1.3.0 read only this tree. However late one comes online, it
# updates to VERSION, which reads the new tree from then on. A binary already
# signed in the legacy tree is kept as it is.
#
# Usage: scripts/freeze-legacy.sh <version>
set -euo pipefail
cd "$(dirname "$0")/.."

version="${1:?usage: freeze-legacy.sh <version>}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "version is not MAJOR.MINOR.PATCH: '$version'" >&2
    exit 1
fi
SOURCE="gs://ironflock-downloads/device-agent"
LEGACY="gs://re-agent"

objects=$(gsutil ls "${SOURCE}/*/*/${version}/*" 2>/dev/null || true)
if [[ -z "$objects" ]]; then
    echo "no binaries staged at ${SOURCE}/*/*/${version}/" >&2
    exit 1
fi

copied=0
while IFS= read -r obj; do
    rest=${obj#"$SOURCE"/}
    IFS=/ read -r os arch ver file <<< "$rest"
    case "$file" in
    device-agent) legacy_name=reagent ;;
    device-agent.exe) legacy_name=reagent.exe ;;
    *) continue ;;
    esac
    dest="${LEGACY}/${os}/${arch}/${ver}/${legacy_name}"
    if gsutil -q stat "${dest}.sig"; then
        echo "kept:   ${dest} (signed already)"
        continue
    fi
    gsutil -q -h "Cache-Control:public, max-age=0" cp "$obj" "$dest"
    gsutil -q -h "Cache-Control:public, max-age=0" cp "${obj}.sha256" "${dest}.sha256"
    echo "copied: ${dest}"
    copied=$((copied + 1))
done <<< "$objects"

scripts/sign-release.sh --legacy "$version"
scripts/publish-latest-alias.sh "$LEGACY" "$version"

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
jq -n --arg v "$version" '{production: $v, test: $v, local: $v, all: $v}' > "$work/availableVersions.json"
printf '%s\n' "$version" > "$work/version.txt"
gsutil -q -h "Cache-Control:public, max-age=0" cp "$work/version.txt" "${LEGACY}/version.txt"
gsutil -q -h "Cache-Control:public, max-age=0" cp "$work/availableVersions.json" "${LEGACY}/availableVersions.json"
echo "==> ${LEGACY} frozen at ${version} (${copied} binaries copied)"
