#!/bin/sh
set -eu

channel="${1:?channel required}"
base="${DVR_HUB_PORT_BASE:-9100}"
bind="${DVR_HUB_BIND:-127.0.0.1}"
port="$((base + channel))"

set -- /usr/local/bin/legacybridge \
  --subscribe "${bind}:${port}" \
  --reconnect "${DVR_RECONNECT:-3s}"

if [ "${DVR_VERBOSE:-0}" = "1" ]; then
  set -- "$@" --verbose
fi

if [ "${DVR_LOG_JSON:-0}" = "1" ]; then
  set -- "$@" --log-json
fi

exec "$@"
