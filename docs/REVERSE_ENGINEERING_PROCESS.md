# Reverse Engineering Process (Case Study)

This document describes the technical process used to analyze and replicate a legacy SmartMEye/Life DVR protocol, and to integrate live streaming into go2rtc/Home Assistant.

Purpose:
- provide a reusable reverse engineering workflow for others;
- document practical engineering decisions and trade-offs;
- serve as a portfolio/CV case study.

## 0) Personal background and motivation

Project context:
- developed as a personal engineering project during Computer Engineering studies;
- practical need: keep a functioning legacy DVR in use;
- technical constraint: original software stack is deprecated (`SmartMEye` + ActiveX web plugin).

End goal:
- extract and expose video streams using modern tooling (go2rtc/Home Assistant), without dependency on obsolete clients.

## 1) Initial problem

Starting conditions:
- proprietary protocol, partially undocumented;
- legacy browser plugin dependency (ActiveX);
- no ready public implementation for this exact DVR profile.

## 2) Strategy

Incremental approach in four phases:

1. Passive observation
- capture traffic while using the original client;
- compare single-camera and multi-camera sessions.

2. Minimal replication
- replicate login/bootstrap;
- open a live stream and collect raw media bytes.

3. Media validation
- reconstruct Annex-B H.264;
- refine parser/state machine until playback is stable.

4. Product integration
- build a Go bridge with stdout H.264 output;
- integrate with go2rtc `exec` sources;
- add Docker workflow and diagnostics.

## 3) Protocol reverse engineering

### 3.1 Frame format

Identified:
- magic `5a 5a aa 55`;
- 32-byte base header (`cmd`, `seq/frame_type`, `flag`, `session`, `extra`, `payload_len`).

For media, observed extended packets with 44-byte framing (32 + 12 metadata), payload length at offset 40.

### 3.2 Live command flow

Observed command sequence:
- XML login;
- bootstrap commands + keepalive;
- stream channel request;
- channel/audio open requests.

A notable finding was the presence of two command-id families (legacy and newer mapping) with similar semantics.

### 3.3 Media ACK behavior

Critical for stable streaming:
- first media frame delivers `socket_id`;
- client ACKs initial socket frame;
- client continues ACKing media packets to keep DVR in sync.

## 4) Video decoding path: from gray screen to usable stream

Initial symptom:
- bytes were flowing, but decoded image was gray/corrupted.

Fixes that made stream stable:
- classify frame types correctly (start/continuation/meta/socket-id);
- synchronize on key/config frames before forwarding;
- apply correct payload prefix stripping;
- handle continuation packets consistently;
- ignore keepalive/meta packets in H.264 output path.
- in hub mode, forward `frame_type=2` continuation payloads (instead of dropping them), otherwise output degrades to sparse keyframes.

Result:
- stable live H.264 stream consumable by go2rtc.

## 5) Software integration

### 5.1 Go bridge

Implemented features:
- command + media connection lifecycle;
- login/bootstrap/keepalive handling;
- per-channel stream opening;
- proprietary media parsing;
- H.264 stdout output;
- reconnect and runtime tuning flags.

### 5.2 go2rtc integration

Implemented pattern:
- one `exec` source per channel (`dvr_cam1..dvr_cam5`);
- `main/sub/auto` profiles;
- configurable protocol-channel mapping.
- single-session hub + local subscribers as default for multi-camera stability.

### 5.3 Docker operations

Operational additions:
- ready-to-run compose setup;
- `.env.example` for configuration;
- healthcheck;
- diagnostics helper script.
- guided `.env.example` with explicit mapping-priority notes (`DVR_CHANNEL_MAP` over `DVR_PROTOCOL_OFFSET`);
- hub diagnostics wiring (`DVR_DIAG_FILE`) for frame/drop troubleshooting in single-session mode.

## 6) Playback status

Playback-specific work completed:
- DLL string/symbol inspection for playback-related XML names;
- probe script to test query/playback command variants (`src/python/playback_probe.py`).

Current status:
- live streaming solved;
- playback not finalized yet, pending a full packet capture of a real "Remote Playback" session from original client.

## 7) Security and publication decisions

Before publication:
- excluded captures/artifacts/secrets from the public repository;
- added strict `.gitignore` rules for `.env`, captures, binary outputs;
- kept only placeholder credentials in examples;
- recommended immediate credential rotation.

## 8) Skills demonstrated (portfolio/CV)

Technical skills evidenced:
- TCP binary/XML protocol reverse engineering;
- Wireshark/tshark traffic analysis;
- robust streaming parser design and resynchronization;
- H.264 bitstream troubleshooting;
- Go implementation for real-time I/O;
- integration with self-hosted video stack (go2rtc/Home Assistant);
- secure open-source publication hygiene.

Example CV bullet (EN):
- "Reverse-engineered a legacy DVR protocol (SmartMEye) from packet captures and ActiveX binaries, delivering a Go bridge for stable multi-channel H.264 live streaming in go2rtc/Home Assistant."

## 9) Reusable workflow for other legacy devices

Practical checklist:
1. collect focused captures (single-action and multi-action sessions);
2. map framing and heartbeat behavior;
3. replicate minimum login/bootstrap;
4. validate media with real decoder output (`ffprobe`/`ffplay`);
5. add ACK/state-machine logic for stream stability;
6. expose a simple output interface first (`stdout`);
7. productize afterwards (packaging, observability, docs).

## 10) Next steps

- capture and decode a full playback session from original client;
- finalize playback implementation in Go bridge;
- add offline replay tests for parser regression coverage;
- maintain a firmware-version-aware protocol specification.
