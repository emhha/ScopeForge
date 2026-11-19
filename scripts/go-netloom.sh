#!/bin/sh
# Keep the canonical Go module path while fetching its repository from the private mirror.
set -eu
go_binary=$1
shift
netloom_repository=${NETLOOM_REPOSITORY:-https://192.168.1.100:10002/emhha/netloom}
netloom_module=github.com/emhha/netloom
export GOPRIVATE="${GOPRIVATE:+${GOPRIVATE},}${netloom_module}"
export GONOPROXY="${GONOPROXY:+${GONOPROXY},}${netloom_module}"
export GONOSUMDB="${GONOSUMDB:+${GONOSUMDB},}${netloom_module}"
netloom_config_count=${GIT_CONFIG_COUNT:-0}
case "$netloom_config_count" in ''|*[!0-9]*) echo 'Invalid GIT_CONFIG_COUNT' >&2; exit 2;; esac
export "GIT_CONFIG_KEY_${netloom_config_count}=url.${netloom_repository}.insteadOf"
export "GIT_CONFIG_VALUE_${netloom_config_count}=https://${netloom_module}"
export GIT_CONFIG_COUNT=$((netloom_config_count + 1))
exec "$go_binary" "$@"
