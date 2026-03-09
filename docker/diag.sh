#!/bin/sh
set -eu

container="${1:-go2rtc-legacybridge}"
api_url="${API_URL:-http://127.0.0.1:1984}"

echo "== Container =="
docker ps --filter "name=${container}" --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'
echo

echo "== go2rtc streams =="
streams_json="$(curl -fsS "${api_url}/api/streams" || true)"
if [ -z "${streams_json}" ]; then
  echo "API non raggiungibile: ${api_url}/api/streams"
else
  STREAMS_JSON="${streams_json}" python3 - <<'PY'
import json
import os

raw = os.environ.get("STREAMS_JSON", "{}")
data = json.loads(raw)
for name in sorted(data.keys()):
    stream = data[name] or {}
    producers = stream.get("producers") or []
    consumers = stream.get("consumers") or []
    active = [p.get("source") or p.get("url") for p in producers if isinstance(p, dict)]
    active = [x for x in active if x]
    print(f"{name}: producers={len(producers)} consumers={len(consumers)}")
    if active:
        print(f"  active={active[0]}")
PY
fi
echo

echo "== Ultimi log container =="
docker logs --tail 40 "${container}" 2>&1 || true
echo

echo "== Ultime righe diag bridge =="
docker exec "${container}" sh -lc '
if [ -f /config/diag_cam1.log ]; then
  tail -n 40 /config/diag_cam1.log
else
  echo "/config/diag_cam1.log non trovato"
fi
' 2>/dev/null || true
