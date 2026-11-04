#!/usr/bin/env bash
# Authenticode-sign Windows PE binaries in place with jsign, with an RFC 3161
# timestamp from a public timestamp authority.
#
#   scripts/sign-windows.sh release <pe-file>...
#       The key is the Cloud KMS key ironflock-release/agent-codesign; it
#       cannot leave KMS. KMS_ACCESS_TOKEN is an access token of agent-signer,
#       the identity that may only sign, and only from a release run of
#       release.yml on main. The certificate chain is the committed signer
#       certificate and the root agents pin (docs/WINDOWS-CODE-SIGNING.md).
#   scripts/sign-windows.sh dry-run <pe-file>...
#       Release pull requests and manual runs: the same jsign call with a
#       throwaway root and signer made here. The throwaway root goes to
#       $CODESIGN_ROOT, for `just codesign-check`.
#
# Check the result with `just codesign-check <pe-file>...`: release.yml does,
# before it publishes; sign-release.sh again, before a version goes live.
set -euo pipefail

JSIGN_VERSION=7.6
JSIGN_SHA256=6cf58aa080e7ff82656f320cff8d4a3bff30408df8766806dcba3740dd839fa5
KMS_KEYRING=projects/record-1283/locations/europe-west1/keyRings/ironflock-release
KMS_KEY_VERSION=agent-codesign/cryptoKeyVersions/1
SIGNER_CERT=src/codesign/signer/agent-codesign.crt
ROOT_CERT=src/codesign/roots/ironflock-root.crt
TSA_URLS=http://timestamp.digicert.com,http://timestamp.sectigo.com

usage="usage: scripts/sign-windows.sh <release|dry-run> <pe-file>..."
mode="${1:?$usage}"
shift
[ "$#" -gt 0 ] || { echo "$usage" >&2; exit 1; }
for f in "$@"; do
    [ -f "$f" ] || { echo "$f: no such file" >&2; exit 1; }
done

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

case "$mode" in
release)
    : "${KMS_ACCESS_TOKEN:?release signing needs KMS_ACCESS_TOKEN (agent-signer)}"
    if [ ! -s "$SIGNER_CERT" ]; then
        echo "$SIGNER_CERT is missing: issue it first (docs/WINDOWS-CODE-SIGNING.md)" >&2
        exit 1
    fi
    cat "$SIGNER_CERT" "$ROOT_CERT" >"$work/chain.pem"
    # The token stays out of the command line: jsign reads it from the
    # environment (env:).
    key=(--storetype GOOGLECLOUD --keystore "$KMS_KEYRING" --storepass env:KMS_ACCESS_TOKEN
        --alias "$KMS_KEY_VERSION" --certfile "$work/chain.pem")
    ;;
dry-run)
    : "${CODESIGN_ROOT:?a dry run needs CODESIGN_ROOT (where the throwaway root goes)}"
    subject="O=IronFlock (dry run)"
    openssl req -x509 -newkey rsa:3072 -nodes -keyout "$work/root.key" -out "$work/root.crt" -days 60 \
        -subj "/CN=Dry Run Code Signing Root/$subject" \
        -addext "keyUsage=critical,keyCertSign,cRLSign" -addext "basicConstraints=critical,CA:TRUE" 2>/dev/null
    openssl req -newkey rsa:3072 -nodes -keyout "$work/signer.key" -out "$work/signer.csr" \
        -subj "/CN=Dry Run Device Agent/$subject" 2>/dev/null
    printf 'keyUsage=critical,digitalSignature\nextendedKeyUsage=codeSigning\n' >"$work/signer.ext"
    openssl x509 -req -in "$work/signer.csr" -CA "$work/root.crt" -CAkey "$work/root.key" -CAcreateserial \
        -days 60 -extfile "$work/signer.ext" -out "$work/signer.crt" 2>/dev/null
    openssl pkcs12 -export -name codesign -inkey "$work/signer.key" -in "$work/signer.crt" \
        -certfile "$work/root.crt" -passout pass:dry-run -out "$work/signer.p12"
    cp "$work/root.crt" "$CODESIGN_ROOT"
    key=(--storetype PKCS12 --keystore "$work/signer.p12" --storepass dry-run --alias codesign)
    ;;
*)
    echo "$usage" >&2
    exit 1
    ;;
esac

jar="$work/jsign.jar"
curl -fsSL --retry 3 -o "$jar" "https://github.com/ebourg/jsign/releases/download/$JSIGN_VERSION/jsign-$JSIGN_VERSION.jar"
if [ "$(sha256_of "$jar")" != "$JSIGN_SHA256" ]; then
    echo "jsign-$JSIGN_VERSION.jar does not have the pinned SHA-256: not signing" >&2
    exit 1
fi

for f in "$@"; do
    java -jar "$jar" "${key[@]}" --alg SHA-256 \
        --tsaurl "$TSA_URLS" --tsmode RFC3161 --tsretries 3 --tsretrywait 10 \
        --name "IronFlock Device Agent" --url https://ironflock.com "$f"
    echo "signed $f ($mode)"
done
