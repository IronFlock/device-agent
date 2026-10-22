#!/usr/bin/env bash
# Assert the frp version agrees across every place it is declared in this
# repository: the frp/ module that builds frpc, the agent's runtime constant
# (frpc update/extraction), and the Justfile. The tunnel repository holds the
# server version (its frp/ module) and MUST be kept equal by hand — this check
# cannot see another repository.
set -euo pipefail

# The recipe file is tracked as "Justfile"; resolve case-robustly so this works
# on both case-sensitive (Linux CI) and case-insensitive (macOS) filesystems.
if [ -f Justfile ]; then
    justfile_path="Justfile"
elif [ -f justfile ]; then
    justfile_path="justfile"
else
    echo "ERROR: no Justfile/justfile found" >&2
    exit 1
fi

embedded=$(grep -oE 'FRP_VERSION = "[^"]+"' src/embedded/frpc.go | cut -d'"' -f2)
module=$(awk '$1 == "github.com/fatedier/frp" { sub(/^v/, "", $2); print $2 }' frp/go.mod)
justf=$(grep -oE 'FRP_VERSION := "[^"]+"' "$justfile_path" | cut -d'"' -f2)

echo "frp/go.mod:           $module"
echo "src/embedded/frpc.go: $embedded"
echo "$justfile_path:             $justf"

if [ -z "$module" ] || [ "$embedded" != "$module" ] || [ "$embedded" != "$justf" ]; then
    echo "ERROR: frp versions disagree across the repo" >&2
    exit 1
fi

echo "OK: frp is $embedded everywhere in this repo"
echo "REMINDER: the tunnel repository's frp/go.mod must also be v$embedded"
