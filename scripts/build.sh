#!/bin/sh

src_path=$(realpath "$1")
target_path=$(realpath "$2")
target_string="$3"

target_os=$(echo $target_string | cut -d "/" -f 1)
target_arch=$(echo $target_string | cut -d "/" -f 2)
target_arch_variant=$(echo $target_string | cut -d "/" -f 3)
build_arch="$target_arch"

if [ -z "$target_arch" ]; then
    echo "the first argument should be the target architecture"
    exit 1
fi

if [ -z "$target_os" ]; then
    echo "the second argument should be the target operating system"
    exit 1
fi

go_version=$(go version >/dev/null 2>&1)
if [ "$?" -ne 0 ]; then
    echo "go is not installed"
    exit 1
fi

combination=$(go tool dist list | grep $target_os/$target_arch)
if [ -z "$combination" ]; then
    echo "the given combination of architecture ($target_arch) and OS ($target_os) is not supported"
    exit 1
fi

possible_combinations=$(echo "$combination" | wc -l | awk '{ print $target_os }')
if [ "$possible_combinations" -ne 1 ] && [ $target_arch != "arm" ]; then
    echo "the given combination of architecture ($target_arch) and OS ($target_os) is not supported"
    exit 1
fi

export GOOS="$target_os"
export GOARCH="$target_arch"
export CGO_ENABLED=0

if [ "$target_arch" = "arm" ]; then
    if [ -z "$target_arch_variant" ]; then
        echo "when specifying arm the architecture variant cannot be empty"
        exit 1
    fi

    build_arch="${target_arch}v${target_arch_variant}"
    export GOARM="$target_arch_variant"
fi

embedded_dir="${src_path}/embedded"
mkdir -p "$embedded_dir"

# Skip frpc for Windows - tunnels are not supported on Windows
if [ "$target_os" = "windows" ]; then
    echo "Skipping frpc for Windows (tunnels not supported)"
    # Create empty placeholder for go:embed directive
    : > "$embedded_dir/frpc_binary"
    echo "Created empty frpc_binary placeholder for Windows build"
else
    # frpc for the same target (GOARM is already exported for arm), built from
    # source by the frp/ module — see scripts/build-frpc.sh.
    "$(dirname "$0")/build-frpc.sh" "$target_os" "$target_arch" "$embedded_dir/frpc_binary" || {
        echo "Failed to build frpc for ${target_string}"
        exit 1
    }
    echo "Embedded frpc binary at ${embedded_dir}/frpc_binary (size: $(wc -c < "${embedded_dir}/frpc_binary") bytes)"
fi

# EXTRA_LDFLAGS: test builds only (device-init's e2e: release.versionOverride,
# release.TestFault).
prefix="device-agent"
binary_name="$prefix-$target_os-$target_arch"
if [ -n "$target_arch_variant" ]; then
    binary_name="$prefix-$target_os-${target_arch}v${target_arch_variant}"
fi

echo "Building device-agent for ${target_os}/${build_arch}..."
cd $src_path && go build -v -a -ldflags "-X 'reagent/release.BuildArch=$build_arch' ${EXTRA_LDFLAGS:-}" -o "$target_path/$binary_name"
build_status=$?

# Cleanup embedded binary after build (don't let this mask a build failure: the
# script's exit code must reflect `go build`, or CI's build step passes on a
# broken build and the failure only surfaces later — e.g. at the SBOM step).
rm -f "$embedded_dir/frpc_binary"

exit $build_status

