#!/usr/bin/env bash
# Alias one version's binaries to <tree>/<os>/<arch>/latest/ (see the Justfile's
# publish-latest-alias for why), refusing when it is not staged or not signed.
#
# Usage: scripts/publish-latest-alias.sh <gs://bucket/tree> <version>
set -euo pipefail

tree="${1:?usage: publish-latest-alias.sh <gs://bucket/tree> <version>}"
version="${2:?usage: publish-latest-alias.sh <gs://bucket/tree> <version>}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "version is not MAJOR.MINOR.PATCH: '$version'" >&2
    exit 1
fi
# <os>/<arch>/<version>/<file> — four segments, so the five-segment frpc sub-tree
# (<tree>/frpc/<os>/<arch>/<frpver>/frpc) can never match this glob.
objects=$(gsutil ls "${tree}/*/*/${version}/*" 2>/dev/null || true)
if [[ -z "$objects" ]]; then
    echo "no binaries staged at ${tree}/*/*/${version}/ — did the release CI run?" >&2
    exit 1
fi
unsigned=""
while IFS= read -r obj; do
    case "$obj" in ""|*.sha256|*.sig) continue ;; esac
    grep -qxF "${obj}.sig" <<< "$objects" || unsigned="${unsigned}    ${obj}"$'\n'
done <<< "$objects"
if [[ -n "$unsigned" ]]; then
    echo "not aliasing ${version}: no update signature (.sig) next to" >&2
    printf '%s' "$unsigned" >&2
    exit 1
fi
echo "==> aliasing ${version} -> latest in ${tree}"
count=0
# read -r line-by-line rather than `for obj in $objects`: that would lean on IFS word
# splitting, which also splits on spaces.
while IFS= read -r obj; do
    [[ -n "$obj" ]] || continue
    rest=${obj#"$tree"/}
    os=${rest%%/*}; rest=${rest#*/}
    arch=${rest%%/*}; rest=${rest#*/}
    ver=${rest%%/*}; file=${rest#*/}
    [[ "$ver" == "$version" ]] || continue
    # Cache-Control is set on the copy itself: the alias objects are new.
    gsutil -q -h "Cache-Control:public, max-age=0" cp "$obj" "${tree}/${os}/${arch}/latest/${file}"
    echo "    ${os}/${arch}/${file}"
    count=$((count + 1))
done <<< "$objects"
if [[ "$count" -eq 0 ]]; then
    echo "matched no <os>/<arch>/${version}/<file> objects — bucket layout changed?" >&2
    exit 1
fi
echo "==> aliased ${count} object(s) to latest"
