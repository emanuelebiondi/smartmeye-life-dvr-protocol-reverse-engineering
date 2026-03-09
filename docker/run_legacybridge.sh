#!/bin/sh
set -eu

channel="${1:?channel required}"
stream_override="${2:-}"
# Dai dump NEW: cmd media 0..7, quindi cam utente 1..N => protocollo (channel + offset).
# Default offset -1: cam1->0, cam2->1, ...
proto_offset="${DVR_PROTOCOL_OFFSET:--1}"
proto="$((channel + proto_offset))"
stream="${DVR_STREAM:-0}"
if [ -n "${stream_override}" ]; then
  stream="${stream_override}"
fi

set -- /usr/local/bin/legacybridge \
  --host "${DVR_IP}" \
  --cmd-port "${DVR_CMD_PORT:-6001}" \
  --data-port "${DVR_DATA_PORT:-6002}" \
  --stream "${stream}" \
  --keepalive "${DVR_KEEPALIVE:-1s}" \
  --reconnect "${DVR_RECONNECT:-3s}" \
  --user "${DVR_USER}" \
  --pass "${DVR_PASSWORD}" \
  --channel "${channel}" \
  --protocol-channel "${proto}"

if [ -n "${DVR_DIAG_FILE:-}" ]; then
  set -- "$@" --diag-file "${DVR_DIAG_FILE}"
fi

if [ "${DVR_VERBOSE:-0}" = "1" ]; then
  set -- "$@" --verbose
fi

exec "$@"
