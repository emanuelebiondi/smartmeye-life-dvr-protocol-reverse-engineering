# DVR Protocol and Device Notes

## Device

- **Brand/Model**: Life D/N/I 2013
- **Vendor**: Life Electronics S.p.A. (Italy)
- **Client app**: SmartMEye (XMEye is not compatible with this specific device)
- **Protocol family**: DVRIP / Sofia-like stack
- **Magic bytes**: `5a 5a aa 55`
- **Ports**:
  - `6001`: command/control
  - `6002`: media stream

Useful references:
- [DVRIP library](https://github.com/alexshpilkin/dvrip)
- [python-dvr](https://github.com/xyyangkun/python-dvr)

## Packet structure

Each frame starts with a 32-byte header:

| Offset | Length | Field |
|--------|--------|-------|
| 0 | 4 | Magic `5a 5a aa 55` - start_cmd|
| 4 | 4 | `cmd` (LE) |
| 8 | 4 | `seq` (LE) |
| 12 | 4 | `flag` (LE) |
| 16 | 4 | `session` (LE) |
| 20 | 4 | `extra` (LE) |
| 24 | 4 | reserved |
| 28 | 4 | `payload_len` (LE) |
| 32 | N | payload (N = payload_len)|

## Port 6001 (command channel)

Observed flow:
- login request (`cmd=0x56F5` family or equivalent new family cmd);
- bootstrap sequence (multiple no-payload commands + keepalive);
- device/config responses (`dev_info`, `ch_attri_cfg`, `ch_basic_cfg`, events);
- stream open sequence:
  - `stream_ch_request`
  - `open_channel_request`
  - `open_audio_request`

## Port 6002 (media channel)

Observed behavior:
- first frame contains a 4-byte `socket_id` (`seq=800` pattern);
- client sends initial ACK with that `socket_id`;
- media packets follow with per-channel `cmd` and frame type semantics.

Important implementation details:
- media framing often uses an extended 44-byte header (32 + 12 metadata bytes), with payload length at offset 40;
- H.264 payload is usually found after a 12-byte prefix;
- periodic media ACKs from client to DVR are required to keep stream synchronization stable.

## H.264 extraction notes

From analyzed captures:
- a 12-byte payload prefix is present on many start and continuation packets;
- Annex-B start codes (`00 00 00 01` or `00 00 01`) appear after that prefix;
- stable decoding requires waiting for key/config sync and handling continuation packets correctly.

## Local analysis workflow

With `tshark`:

```bash
python3 src/python/analyze_smartmeyi_pcap.py captures/dump1.pcapng
python3 src/python/analyze_dumps.py captures/dump1.pcapng captures/dump2.pcapng
python3 src/python/analyze_new_captures.py captures/dump_single.pcapng captures/dump_multi.pcapng
```

To extract a playable stream candidate:

```bash
python3 src/python/create_valid_h264.py captures/dump1.pcapng --port 6002 --channel 5 -o channel5.h264
ffplay channel5.h264
```
