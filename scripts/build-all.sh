#!/bin/sh
# Explicit toolchains prevent CGO builds from silently producing unusable stubs.
set -eu
go_binary=${GO:-go}
host_os=$("$go_binary" env GOHOSTOS)
host_arch=$("$go_binary" env GOHOSTARCH)
for target in "$@"; do
    case "$target" in linux/amd64|linux/arm64|windows/amd64|windows/arm64|darwin/amd64|darwin/arm64) ;;
        *) echo "Unsupported target: $target" >&2; exit 2;; esac
    key=CC_$(printf '%s' "$target" | tr / _)
    compiler=$(printenv "$key" || true)
    if [ -z "$compiler" ] && [ "$target" = "$host_os/$host_arch" ]; then
        compiler=${CC:-$("$go_binary" env CC)}
    fi
    if [ -z "$compiler" ] || ! command -v "$compiler" >/dev/null 2>&1; then
        echo "CGO compiler missing for $target. Set $key to a target C compiler executable; see README.md." >&2
        exit 2
    fi
done
make embed-web
for target in "$@"; do
    build_os=${target%/*}; build_arch=${target#*/}; build_ext=
    key=CC_$(printf '%s' "$target" | tr / _)
    compiler=$(printenv "$key" || true)
    if [ -z "$compiler" ]; then compiler=${CC:-$("$go_binary" env CC)}; fi
    if [ "$build_os" = windows ]; then build_ext=.exe; fi
    CC="$compiler" make build-embedded GOOS="$build_os" GOARCH="$build_arch" \
        BINARY="dist/scopeforge-$build_os-$build_arch$build_ext"
done
