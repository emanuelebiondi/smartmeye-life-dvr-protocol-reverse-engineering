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
- `--keepalive` keepalive interval (default `1s`)
- `--reconnect` reconnect delay (default `3s`)
- `--verbose` verbose stderr logging
- `--diag-file` media diagnostics output

## Channel mapping

Common mapping observed:
- `channel 1 -> protocol 0`
- `channel 2 -> protocol 1`
- `channel 3 -> protocol 2`
- `channel 4 -> protocol 3`
- `channel 5 -> protocol 4`

Equivalent formula:
- `protocol-channel = channel - 1` (with `--channel-base 1`)

If your firmware uses a different mapping, set `--protocol-channel` explicitly.

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
- `dvr_camX_main`
- `dvr_camX_sub`
- `dvr_camX_auto`

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
- enable `--verbose`;
- use `--diag-file` to inspect frame type/drop/sync behavior;
- confirm Annex-B start codes (`00 00 00 01`) in generated output.

Important:
- keep `stdout` for video only;
- keep logs on `stderr`.

## Playback note

Remote playback is not fully finalized in the Go bridge yet.
For playback protocol probing, use:
- `src/python/playback_probe.py`

See full context in:
- `docs/REVERSE_ENGINEERING_PROCESS.md`
