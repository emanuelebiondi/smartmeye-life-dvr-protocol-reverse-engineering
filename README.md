# SmartMeye Life DVR Protocol Reverse Engineering

Bridge and tooling to extract H.264 video from a legacy DVR that uses **DVRIP** with the **SmartMeye** app.  
Magic: `5a 5a aa 55`, ports: `6001` (command), `6002` (media).

- **Device**: Life D/N/I 2013 (Life Electronics S.p.A.)
- **Protocol notes**: [docs/PROTOCOLLO_E_DEVICE.md](docs/PROTOCOLLO_E_DEVICE.md)
- **Full reverse engineering case study**: [docs/REVERSE_ENGINEERING_PROCESS.md](docs/REVERSE_ENGINEERING_PROCESS.md)

## Background and motivation

This project started as a personal engineering effort during my Computer Engineering studies.

Main goals:
- keep a still-working legacy DVR in service instead of discarding it;
- remove dependency on deprecated software (`SmartMeye`) and deprecated browser technology (ActiveX).

The key challenge was reverse engineering a proprietary protocol with no ready-to-use public implementation for this exact DVR family.

## Quick start (Docker)

```bash
cd docker
cp .env.example .env
docker compose up -d --build
./diag.sh
```

Available active go2rtc stream profiles:
- `dvr_cam1..dvr_cam5` (hub subscriber mode)

Additional direct profiles (`*_main`, `*_sub`, `*_auto`) are kept as commented documentation in
`docker/config/go2rtc.yaml`.
Channels `6..8` are documented/commented and can be enabled if those inputs are used.
Legacy single-instance/direct mode is still supported for debugging and compatibility.

## Hub Architecture (Single DVR Session)

To keep multi-camera view stable on fragile legacy DVRs, this stack uses:

- one `legacyhub` process with a single DVR session (`6001/6002`);
- local per-channel subscriber endpoints;
- go2rtc `exec` sources attached to those local subscribers.

Stream profile selection in hub mode is global:
- `DVR_STREAM=0` -> main stream;
- `DVR_STREAM=1` -> sub stream.

So `dvr_cam1..8` all use the same profile selected in `.env`.

Recommendation:
- use hub mode for multi-camera usage;
- use direct mode only for targeted diagnostics or firmware compatibility checks.

Channel count note:
- the DVR protocol/device supports up to 8 channels;
- in this deployment only channels 1..5 are enabled by default;
- to enable more, update `DVR_HUB_CHANNELS` and uncomment `dvr_cam6..dvr_cam8` in `go2rtc.yaml`.
- optional explicit mapping is available with `DVR_CHANNEL_MAP` (example: `1:0,2:1,3:2,4:3,5:4`).
- when `DVR_CHANNEL_MAP` is set, it takes priority over `DVR_PROTOCOL_OFFSET`.

## Project structure

- `legacybridge/`: Go bridge (legacy DVR -> H.264 on stdout)
- `docker/`: compose stack, go2rtc config, Dockerfile, diagnostics
- `src/python/`: protocol analysis and extraction scripts
- `docs/`: protocol and reverse engineering documentation

`captures/` and `artifacts/` are intentionally excluded from this public repository.

## Main scripts

| Script | Purpose |
|--------|---------|
| `src/python/analyze_smartmeyi_pcap.py` | Frame-level protocol inspection (6001/6002) |
| `src/python/analyze_dumps.py` | Batch analysis of multiple captures |
| `src/python/analyze_new_captures.py` | Compare single-cam vs multi-cam captures |
| `src/python/create_valid_h264.py` | Build playable H.264 from capture data |
| `src/python/playback_probe.py` | Probe playback/query XML variants |

## Capture analysis examples

With `tshark` installed:

```bash
# Single capture
python3 src/python/analyze_smartmeyi_pcap.py captures/my_dump.pcapng

# Multiple captures
python3 src/python/analyze_dumps.py captures/dump1.pcapng captures/dump2.pcapng captures/dump3.pcapng
```

## Go bridge usage

See [legacybridge/README.md](legacybridge/README.md) for build, runtime flags, and go2rtc integration details.

## Playback status

Playback is not fully finalized in the Go bridge yet.
A dedicated probe is available:

```bash
# 1) Query probe
python3 src/python/playback_probe.py \
  --host 192.168.1.10 --user Admin --password 'PASSWORD' \
  --channel 1 --channel-base 1 \
  scan --day 2026-03-09

# 2) Best-effort playback attempt + H264 dump
python3 src/python/playback_probe.py \
  --host 192.168.1.10 --user Admin --password 'PASSWORD' \
  --channel 1 --channel-base 1 \
  play \
  --start-time '2026-03-09 00:00:00' \
  --stop-time '2026-03-09 00:10:00' \
  --seconds 20 \
  --out playback_probe.h264
```

If output remains `0` bytes, the DVR likely rejected the XML/cmd variant. In that case, a dedicated Wireshark capture of an original "Remote Playback" session is needed.

## Changelog

- 2026-03-10
  - Added: single-session hub architecture (`legacyhub`) with local subscriber mode.
  - Added: `--hub` and `--subscribe` runtime modes in `legacybridge`.
  - Added: optional `--channel-map` for explicit user->protocol channel mapping.
  - Added: optional Prometheus metrics endpoint (`--metrics-addr`).
  - Added: optional JSON runtime logs (`--log-json`).
  - Added: parser/config unit tests in `legacybridge/main_test.go`.
  - Changed: Docker stack now runs `legacyhub` + `go2rtc`.
  - Changed: active go2rtc profiles simplified to `dvr_cam1..5` (hub subscriber mode).
  - Fixed: multi-camera instability caused by opening too many direct DVR sessions in parallel.
