#!/bin/sh
set -eu

channel="${1:?channel required}"
base="${DVR_HUB_PORT_BASE:-9100}"
bind="${DVR_HUB_BIND:-127.0.0.1}"
port="$((base + channel))"

exec /usr/local/bin/legacybridge \
  --subscribe "${bind}:${port}" \
  --reconnect "${DVR_RECONNECT:-3s}"
