#!/bin/sh
set -eu

set -- /usr/local/bin/legacybridge \
  --hub \
  --host "${DVR_IP}" \
  --cmd-port "${DVR_CMD_PORT:-6001}" \
  --data-port "${DVR_DATA_PORT:-6002}" \
  --stream "${DVR_STREAM:-0}" \
  --keepalive "${DVR_KEEPALIVE:-1s}" \
  --reconnect "${DVR_RECONNECT:-3s}" \
  --user "${DVR_USER}" \
  --pass "${DVR_PASSWORD}" \
  --hub-bind "${DVR_HUB_BIND:-127.0.0.1}" \
  --hub-port-base "${DVR_HUB_PORT_BASE:-9100}" \
  --hub-channels "${DVR_HUB_CHANNELS:-1,2,3,4,5}" \
  --hub-protocol-offset "${DVR_PROTOCOL_OFFSET:--1}"

if [ "${DVR_VERBOSE:-0}" = "1" ]; then
  set -- "$@" --verbose
fi

exec "$@"
