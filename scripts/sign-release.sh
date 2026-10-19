#!/usr/bin/env bash
# Sign what agents are about to be offered, through OpenBao transit, right
# before the version files flip. Run by `just promote` (and by
# `just publish-version` / `just publish-latestVersions`), so promotion cannot
# skip it; `just sign-release` runs it alone.
#
# CI stages binaries WITHOUT signatures. The private key is the transit key
# agent-update (ed25519, not exportable) and never leaves OpenBao. Signed here:
#   - device-agent[.exe] of every version availableVersions.json names, all
#     targets, in gs://ironflock-downloads/device-agent/;
#   - frpc[.exe] of the FRP_VERSION this tree pins, in device-agent/frpc/;
#   - the manifest: availableVersions.json plus issued_at, sealed into
#     build/manifest.json (an updatesig envelope). `just publish-latestVersions`
#     uploads it; 1.3.0+ agents trust no channel map without it.
# --legacy <version> signs reagent[.exe] of that one version in the frozen
# gs://re-agent tree instead (scripts/freeze-legacy.sh), and no manifest.
#
# For each object: download it, hash it here (the .sha256 sidecar is only
# cross-checked, never trusted), build the message (`sign-update message`),
# sign all messages in ONE kubectl exec session, turn each vault:vN:<sig> into
# a .sig that must verify against the keys compiled into THIS tree, verify
# every .sig once more, and only then upload. A published .sig that already
# verifies is kept, so re-running is cheap and does not churn signatures.
#
# Env: OPENBAO_KUBE_CONTEXT (default: gke_record-1283_europe-west1-b_ironflock-prod)
#      OPENBAO_TRANSIT_KEY_VERSION: sign with this key version (during a key
#      rotation, until the fleet trusts the new one); default: the latest.
set -euo pipefail
cd "$(dirname "$0")/.."

TRANSIT_KEY="agent-update"
MANIFEST_CONTEXT="device-agent-manifest:v1"
MANIFEST_SUBJECT="device-agent/manifest.json"

if [[ "${1:-}" == "--legacy" ]]; then
    legacy=1
    BUCKET="gs://re-agent"
    AGENT_KIND="reagent"
    versions="${2:?usage: sign-release.sh --legacy <version>}"
else
    legacy=0
    BUCKET="gs://ironflock-downloads/device-agent"
    AGENT_KIND="device-agent"
    versions=$(jq -r '[.[]] | unique | .[]' availableVersions.json)
fi

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

for version in $versions; do
    if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "'$version' is not MAJOR.MINOR.PATCH" >&2
        exit 1
    fi
done
frp_version=$(grep -oE 'FRP_VERSION = "[^"]+"' src/embedded/frpc.go | cut -d'"' -f2)

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

# Verification uses the keys compiled into this tree: what the release trusts.
(cd src && go build -o "$work/sign-update" ./cmd/sign-update)
helper="$work/sign-update"

listing=""
for version in $versions; do
    found=$(gsutil ls "$BUCKET/*/*/$version/*" 2>/dev/null || true)
    if [[ -z "$found" ]]; then
        echo "version $version is not staged at $BUCKET/*/*/$version/ — did the release CI run?" >&2
        exit 1
    fi
    listing+="$found"$'\n'
done
listing+="$(gsutil ls "$BUCKET/frpc/*/*/$frp_version/*" 2>/dev/null || true)"$'\n'

objects=()   # gs:// object of each binary ("" for the manifest)
locals=()    # its local copy
targets=()   # its sign-update target flags, space-separated
requests=()  # "<index> <base64 message>" for the ones to sign
n=0
while IFS= read -r object; do
    [[ -n "$object" ]] || continue
    rel=${object#"$BUCKET"/}
    file=${rel##*/}
    case "$file" in
    "$AGENT_KIND" | "$AGENT_KIND.exe" | frpc | frpc.exe) ;;
    *) continue ;;
    esac
    kind=${file%.exe}
    path=$rel
    if [[ "$kind" == frpc ]]; then
        [[ "$rel" == frpc/* ]] || { echo "unexpected frpc object $object" >&2; exit 1; }
        path=${rel#frpc/}
    fi
    IFS=/ read -r os arch version _ <<< "$path"

    local_copy="$work/$n/$file"
    mkdir -p "$work/$n"
    gsutil -q cp "$object" "$local_copy"
    sidecar=$(gsutil cat "$object.sha256" 2>/dev/null | awk '{print $1}' || true)
    actual=$(sha256_of "$local_copy")
    if [[ "$sidecar" != "$actual" ]]; then
        echo "$object: downloaded SHA-256 $actual, sidecar says '${sidecar}' — not signing" >&2
        exit 1
    fi

    objects[n]=$object
    locals[n]=$local_copy
    targets[n]="-kind $kind -os $os -arch $arch -version $version"
    # shellcheck disable=SC2086 # targets[n] is a flag list
    if gsutil -q stat "$object.sig" &&
        gsutil -q cp "$object.sig" "$local_copy.sig" &&
        "$helper" verify ${targets[n]} "$local_copy" >/dev/null 2>&1; then
        echo "kept:   $object.sig (already verifies)"
    else
        rm -f "$local_copy.sig"
        message=$("$helper" message ${targets[n]} "$local_copy")
        requests+=("$n $message")
    fi
    n=$((n + 1))
done <<< "$listing"

if [[ "$n" -eq 0 ]]; then
    echo "nothing to sign: no $AGENT_KIND or frpc objects found" >&2
    exit 1
fi

# The manifest rides the same OpenBao session. Signed fresh every time: its
# issued_at is the moment of promotion.
manifest=-1
if [[ "$legacy" -eq 0 ]]; then
    manifest=$n
    mkdir -p "$work/$n"
    jq -c --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{channels: ., issued_at: $now}' availableVersions.json \
        > "$work/$n/manifest.payload.json"
    objects[n]=""
    locals[n]="$work/$n/manifest.payload.json"
    targets[n]="-context $MANIFEST_CONTEXT -subject $MANIFEST_SUBJECT"
    # shellcheck disable=SC2086
    requests+=("$n $("$helper" message ${targets[n]} "${locals[n]}")")
    n=$((n + 1))
fi

if [[ "${#requests[@]}" -gt 0 ]]; then
    echo "==> signing ${#requests[@]} object(s) with OpenBao transit key $TRANSIT_KEY${OPENBAO_TRANSIT_KEY_VERSION:+ v$OPENBAO_TRANSIT_KEY_VERSION}"
    printf '%s\n' "${requests[@]}" |
        scripts/openbao-admin.sh "$(cat scripts/openbao-transit-sign.sh)" "$TRANSIT_KEY" "${OPENBAO_TRANSIT_KEY_VERSION:-}" \
            > "$work/signatures"

    answered=0
    while read -r id signature; do
        [[ "$id" =~ ^[0-9]+$ && -n "${locals[id]:-}" && ! -e "${locals[id]}.sig" ]] || {
            echo "unexpected line from OpenBao: '$id ...'" >&2
            exit 1
        }
        # shellcheck disable=SC2086
        printf '%s' "$signature" | "$helper" sig-from-openbao ${targets[id]} "${locals[id]}"
        answered=$((answered + 1))
    done < "$work/signatures"
    if [[ "$answered" -ne "${#requests[@]}" ]]; then
        echo "OpenBao answered $answered of ${#requests[@]} signing requests" >&2
        exit 1
    fi
fi

# Every .sig, kept or new, verifies before anything is uploaded.
for ((i = 0; i < n; i++)); do
    # shellcheck disable=SC2086
    "$helper" verify ${targets[i]} "${locals[i]}"
done

for request in "${requests[@]}"; do
    i=${request%% *}
    [[ "$i" -ne "$manifest" ]] || continue
    gsutil -q -h "Cache-Control:public, max-age=0" cp "${locals[i]}.sig" "${objects[i]}.sig"
    echo "signed: ${objects[i]}.sig"
done

if [[ "$manifest" -ge 0 ]]; then
    mkdir -p build
    "$helper" seal -context "$MANIFEST_CONTEXT" -subject "$MANIFEST_SUBJECT" "${locals[manifest]}" > build/manifest.json
    echo "sealed: build/manifest.json ($(jq -c .channels "${locals[manifest]}")); publish-latestVersions uploads it"
fi
echo "==> $n object(s) signed for versions: $(echo $versions) and frpc $frp_version"
