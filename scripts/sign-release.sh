#!/usr/bin/env bash
# Sign what agents are about to be offered, right before the version files
# flip. Run by the promote recipes (`promote-ci` in CI, `promote-bridge` and
# the emergency `promote` by hand), so promotion cannot skip it;
# `just sign-release` runs it alone.
#
# Two signers, neither key exportable (SIGNER):
#   kms      Cloud KMS ironflock-release/agent-update (Ed25519). CI, after the
#            bridge release: release.yml's promote job, which may use the key
#            only as a tag run of device-agent's release workflow.
#   openbao  OpenBao transit agent-update, through `kubectl exec` (needs the
#            production cluster). The bridge release and emergencies only.
#
# CI stages binaries WITHOUT signatures. Signed here:
#   - device-agent[.exe] of every version availableVersions.json names, all
#     targets, in gs://ironflock-downloads/device-agent/;
#   - frpc[.exe] of the FRP_VERSION this tree pins, in device-agent/frpc/;
#   - the channel maps named in MANIFESTS: availableVersions.json plus
#     issued_at, each sealed into build/<name> (an updatesig envelope) and
#     signed as device-agent/<name>. `just publish-latestVersions` uploads
#     them. manifest-v2.json is what agents read; manifest.json is frozen at
#     the bridge release (only `promote-bridge` writes it).
# --legacy <version> signs reagent[.exe] of that one version in the frozen
# gs://re-agent tree instead (scripts/freeze-legacy.sh), and no manifest.
#
# For each object: download it, hash it here (the .sha256 sidecar is only
# cross-checked, never trusted), build the message (`sign-update message`),
# sign it (openbao: all messages in ONE kubectl exec session, each
# vault:vN:<sig> answer; kms: one asymmetric-sign call each), turn each
# signature into a .sig that must verify against the keys compiled into THIS
# tree, verify every .sig once more, and only then upload. A published .sig
# that already verifies is kept, so re-running is cheap and does not churn
# signatures (openbao: only one the OpenBao key made, so that the bridge
# release verifies on agents that trust nothing else).
#
# Env: SIGNER: kms or openbao (default: openbao)
#      MANIFESTS: channel maps to seal (default: manifest-v2.json)
#      KMS_KEY: the Cloud KMS key (default: the production key below)
#      KMS_KEY_VERSION: its version (default: 1)
#      KMS_CREDENTIALS: credential file for the KMS calls only (CI: the
#        agent-signer identity; gsutil keeps the default credentials)
#      OPENBAO_KUBE_CONTEXT (default: gke_record-1283_europe-west1-b_ironflock-prod)
#      OPENBAO_TRANSIT_KEY_VERSION: sign with this key version (during a key
#      rotation, until the fleet trusts the new one); default: the latest.
set -euo pipefail
cd "$(dirname "$0")/.."

TRANSIT_KEY="agent-update"
SIGNER="${SIGNER:-openbao}"
KMS_KEY="${KMS_KEY:-projects/record-1283/locations/europe-west1/keyRings/ironflock-release/cryptoKeys/agent-update}"
KMS_KEY_VERSION="${KMS_KEY_VERSION:-1}"
MANIFESTS="${MANIFESTS:-manifest-v2.json}"
MANIFEST_CONTEXT="device-agent-manifest:v1"
# The OpenBao key, as compiled into the agents before the bridge release. In
# openbao mode an existing .sig is kept only if THIS key made it: the bridge
# release must be verifiable by agents that trust nothing else.
OPENBAO_PUBLIC_KEY="MKstV0PZ93scIuBhtM8DCq0SfbsZR9UU7P9E+621ftU="
keep_key=()
if [[ "$SIGNER" == openbao ]]; then
    keep_key=(-public-key "$OPENBAO_PUBLIC_KEY")
fi
case "$SIGNER" in
kms | openbao) ;;
*) echo "SIGNER must be kms or openbao, not '$SIGNER'" >&2; exit 1 ;;
esac
for manifest_name in $MANIFESTS; do
    case "$manifest_name" in
    manifest.json | manifest-v2.json) ;;
    *) echo "unknown manifest '$manifest_name' in MANIFESTS" >&2; exit 1 ;;
    esac
done

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
        "$helper" verify ${keep_key[@]+"${keep_key[@]}"} ${targets[n]} "$local_copy" >/dev/null 2>&1; then
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

# The channel maps ride the same signing session. Signed fresh every time:
# their issued_at is the moment of promotion.
manifest_ids=()
manifest_names=()
if [[ "$legacy" -eq 0 ]]; then
    issued_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    for manifest_name in $MANIFESTS; do
        mkdir -p "$work/$n"
        jq -c --arg now "$issued_at" '{channels: ., issued_at: $now}' availableVersions.json \
            > "$work/$n/manifest.payload.json"
        objects[n]=""
        locals[n]="$work/$n/manifest.payload.json"
        targets[n]="-context $MANIFEST_CONTEXT -subject device-agent/$manifest_name"
        # shellcheck disable=SC2086
        requests+=("$n $("$helper" message ${targets[n]} "${locals[n]}")")
        manifest_ids+=("$n")
        manifest_names+=("$manifest_name")
        n=$((n + 1))
    done
fi

if [[ "${#requests[@]}" -gt 0 && "$SIGNER" == kms ]]; then
    echo "==> signing ${#requests[@]} object(s) with Cloud KMS $KMS_KEY v$KMS_KEY_VERSION"
    kms_env=()
    if [[ -n "${KMS_CREDENTIALS:-}" ]]; then
        kms_env=(env "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=$KMS_CREDENTIALS")
    fi
    for request in "${requests[@]}"; do
        id=${request%% *}
        printf '%s' "${request#* }" | base64 -d > "$work/$id.message"
        ${kms_env[@]+"${kms_env[@]}"} gcloud kms asymmetric-sign --quiet --version="$KMS_KEY_VERSION" --key="$KMS_KEY" \
            --input-file="$work/$id.message" --signature-file="$work/$id.raw" </dev/null
        # shellcheck disable=SC2086
        base64 < "$work/$id.raw" | tr -d '\n' | "$helper" sig-from-kms ${targets[id]} "${locals[id]}"
    done
elif [[ "${#requests[@]}" -gt 0 ]]; then
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
    [[ -n "${objects[i]}" ]] || continue
    gsutil -q -h "Cache-Control:public, max-age=0" cp "${locals[i]}.sig" "${objects[i]}.sig"
    echo "signed: ${objects[i]}.sig"
done

for k in "${!manifest_ids[@]}"; do
    i=${manifest_ids[k]}
    name=${manifest_names[k]}
    mkdir -p build
    "$helper" seal -context "$MANIFEST_CONTEXT" -subject "device-agent/$name" "${locals[i]}" > "build/$name"
    echo "sealed: build/$name ($(jq -c .channels "${locals[i]}")); publish-latestVersions uploads it"
done
echo "==> $n object(s) signed for versions: $(tr '\n' ' ' <<< "$versions")and frpc $frp_version"
