#!/bin/sh
set -eu

channel="${1:?channel required}"
stream_override="${2:-}"
# From NEW captures: media cmd is 0..7, so user camera 1..N maps to protocol (channel + offset).
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
  --channel "${channel}"

if [ -n "${DVR_CHANNEL_MAP:-}" ]; then
  set -- "$@" --channel-map "${DVR_CHANNEL_MAP}"
else
  set -- "$@" --protocol-channel "${proto}"
fi

if [ -n "${DVR_DIAG_FILE:-}" ]; then
  set -- "$@" --diag-file "${DVR_DIAG_FILE}"
fi

if [ -n "${DVR_METRICS_ADDR:-}" ]; then
  set -- "$@" --metrics-addr "${DVR_METRICS_ADDR}"
fi

if [ "${DVR_VERBOSE:-0}" = "1" ]; then
  set -- "$@" --verbose
fi

if [ "${DVR_LOG_JSON:-0}" = "1" ]; then
  set -- "$@" --log-json
fi

exec "$@"
