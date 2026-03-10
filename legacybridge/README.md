# Legacy Bridge (Go)

`legacybridge` connects to the legacy DVR protocol and writes Annex-B H.264 to `stdout`.
It is designed to be consumed by go2rtc using `exec` sources.

## What it does

- connects to command (`6001`) and media (`6002`) ports;
- performs login, bootstrap, and keepalive;
- opens per-channel stream sessions;
- parses proprietary media packets;
- outputs continuous H.264 video to `stdout`.

## Build

```bash
cd legacybridge
go build -o legacybridge .
```

## Quick run

```bash
./legacybridge \
  --host 192.168.1.10 \
  --user Admin \
  --pass 'PASSWORD' \
  --channel 1 \
  > camera1.h264
```

Validate:

```bash
ffprobe camera1.h264
ffplay camera1.h264
```

## Main runtime flags

- `--host` DVR IP
- `--cmd-port` default `6001`
- `--data-port` default `6002`
- `--user`, `--pass` credentials
- `--channel` user-facing channel index
- `--channel-base` channel base for mapping logic
- `--protocol-channel` explicit protocol channel override
- `--stream` stream profile (`0` main, `1` sub in common setups)
- `--include-seq2` include `frame_type=2` continuation packets (default `true`)
- `--keepalive` keepalive interval (default `1s`)
- `--reconnect` reconnect delay (default `3s`)
- `--verbose` verbose stderr logging
- `--log-json` JSON-line logs on stderr
- `--diag-file` media diagnostics output
- `--channel-map` explicit user:proto mapping (e.g. `1:0,2:1,3:2`)
- `--metrics-addr` expose Prometheus metrics endpoint (e.g. `127.0.0.1:9910`)

## Channel mapping

Common mapping observed:
- `channel 1 -> protocol 0`
- `channel 2 -> protocol 1`
- `channel 3 -> protocol 2`
- `channel 4 -> protocol 3`
- `channel 5 -> protocol 4`

Equivalent formula:
- `protocol-channel = channel - 1` (with `--channel-base 1`)

If your firmware uses a different mapping:
- use `--protocol-channel` for one-off/manual overrides;
- use `--channel-map` for persistent per-channel mapping (e.g. `1:0,2:1,3:2,4:3,5:4`).

Priority:
- explicit `--protocol-channel` has highest priority;
- then `--channel-map`;
- otherwise base/offset mapping is used.

## go2rtc integration

### Docker (recommended)

From repository root:

```bash
cd docker
cp .env.example .env
docker compose up -d --build
./diag.sh
```

Defined stream profiles in `docker/config/go2rtc.yaml`:
- `dvr_cam1..dvr_cam5`
- direct `*_main/sub/auto` examples are present as commented documentation

Single-session hub mode is also available for multi-camera stability:
- `legacyhub` keeps one DVR session open;
- go2rtc streams subscribe locally (`run_legacysubscriber`);
- this avoids opening one DVR session per camera in parallel.
- DVR supports up to 8 channels; enable only channels in use (`DVR_HUB_CHANNELS` + uncomment stream entries).
- `DVR_DIAG_FILE` works in hub mode too (diagnostics emitted by `legacyhub`).
- if the DVR leaves the command socket alive but silently stops sending media, `legacyhub` now reconnects the full DVR session automatically.

Direct single-instance mode is still supported and useful for debugging/compatibility, but hub mode is recommended for multi-camera operation.

### go2rtc standalone

Example `go2rtc.yaml`:

```yaml
streams:
  dvr_cam1: exec:/path/to/legacybridge --host 192.168.1.10 --user Admin --pass 'PASSWORD' --channel 1
  dvr_cam2: exec:/path/to/legacybridge --host 192.168.1.10 --user Admin --pass 'PASSWORD' --channel 2
```

## Troubleshooting

If playback is gray/corrupted:
- verify channel mapping (`--channel-base` / `--protocol-channel`);
- try `--stream 1` (substream);
- ensure continuation packets are enabled (`--include-seq2=true`, default);
- enable `--verbose`;
- use `--diag-file` to inspect frame type/drop/sync behavior;
- confirm Annex-B start codes (`00 00 00 01`) in generated output.

If live view is very slow (for example ~1 frame every 5-6 seconds):
- this is usually caused by dropped continuation packets;
- keep `--include-seq2=true` and update to the latest hub parser version.
- on older hub builds, synchronous media ACK writes could also throttle parsing to roughly one packet every few seconds; current builds rate-limit those ACKs and use a short media-write deadline.
- when testing from go2rtc UI, prefer a single mode URL (for example `stream.html?src=dvr_cam1&mode=webrtc`) to avoid parallel mode consumers during diagnosis.
- some clients auto-fallback to `mse`/`fmp4`; for this legacy bridge, force `webrtc` mode to reduce startup/reopen issues.
- keep `#preload` disabled on unstable DVRs (enable it only if reopen behavior stays stable in your environment).

If hub mode stops producing video while logs still show keepalive traffic:
- this usually means the DVR stalled the media socket but did not close the command session;
- current hub logic treats that as a transport failure and reconnects the whole DVR session after about `15s` without media bytes;
- verify with:

```bash
docker logs legacyhub-bridge | grep -E 'media stalled|hub session ended|hub sync acquired'
```

Home Assistant (`custom:webrtc-camera`) example with forced WebRTC:

```yaml
type: custom:webrtc-camera
server: http://192.168.1.147:1984/
streams:
  - url: dvr_cam1
    mode: webrtc
```

Important:
- keep `stdout` for video only;
- keep logs on `stderr`.

## Playback note

Remote playback is not fully finalized in the Go bridge yet.
For playback protocol probing, use:
- `src/python/playback_probe.py`

See full context in:
- `docs/REVERSE_ENGINEERING_PROCESS.md`

## Changelog

- 2026-03-10
  - Changed: documented the guided, sectioned `docker/.env.example` template and clarified mapping priority (`DVR_CHANNEL_MAP` over `DVR_PROTOCOL_OFFSET`).
  - Changed: `--include-seq2` now defaults to `true`.
  - Fixed: hub mode now forwards `frame_type=2` continuation packets for smooth live FPS.
  - Fixed: hub mode now opens `--diag-file` correctly (`DVR_DIAG_FILE` support in `legacyhub`).
  - Fixed: resolved a deadlock risk in hub subscriber publish/cleanup caused by nested locking during stale connection removal.
  - Fixed: hub publisher now removes disconnected/stale subscriber sockets and avoids reopen lockups after repeated open/close.
  - Fixed: subscriber cleanup now closes sockets explicitly when removed, limiting timeout buildup after many open/close cycles.
  - Fixed: keyframe sync detection now scans multiple NALs (AUD/SEI-prefixed keyframes no longer cause long startup waits).
  - Fixed: hub bootstrap now preserves full keyframe payload (including continuation packets) for faster subscriber startup.
  - Changed: default active go2rtc hub profiles are now stability-first (no `#preload` by default); preload remains optional per channel.
  - Fixed: repeated open/close timeout/loading was caused by all-zero `frame_type=2` continuation payloads being forwarded; these payloads are now dropped.
  - Fixed: reduced hub per-subscriber write timeout to prevent one slow/stale client socket from stalling channel output.
  - Fixed: hub mode now detects silent media-socket stalls (`6002` stops while `6001` keepalive is still active) and reconnects the DVR session automatically.
  - Fixed: media ACK writes on `6002` are now rate-limited with a short deadline; previous synchronous per-packet ACK writes could throttle hub parsing and collapse live FPS after reopen churn.
  - Added: `DVR_SUB_RECONNECT` (`docker/.env`) for faster local subscriber reconnect in hub mode (default `500ms`).
  - Changed: continuation diagnostics now split `wait_sync_cont` (startup/re-sync) from `drop_cont_without_start` (true missing start-frame case).
  - Added: documented known client-mode limitation (auto `mse` selection) and WebRTC-forced configuration for go2rtc UI/Home Assistant.
