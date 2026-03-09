#!/usr/bin/env python3
import argparse
import binascii
import re
import struct
import subprocess
import sys
from dataclasses import dataclass

MAGIC = b"\x5a\x5a\xaa\x55"


@dataclass
class Frame:
    direction: str
    cmd: int
    seq: int
    flag: int
    session: int
    extra: int
    length: int
    payload: bytes


def run_tshark(args):
    try:
        return subprocess.check_output(["tshark", *args], text=True, errors="ignore")
    except FileNotFoundError:
        print("tshark not found in PATH", file=sys.stderr)
        sys.exit(1)


def list_streams(pcap, port):
    out = run_tshark(["-r", pcap, "-T", "fields", "-e", "tcp.stream", "-Y", f"tcp.port=={port}"])
    streams = sorted({int(x) for x in out.split() if x.strip().isdigit()})
    return streams


def follow_stream_hex(pcap, stream_id):
    out = run_tshark(["-r", pcap, "-q", "-z", f"follow,tcp,raw,{stream_id}"])
    chunks = []
    for line in out.splitlines():
        if not re.fullmatch(r"\t?[0-9a-f]+", line):
            continue
        direction = "server->client" if line.startswith("\t") else "client->server"
        data = binascii.unhexlify(line.strip())
        chunks.append((direction, data))
    return chunks


def try_ascii(payload):
    if not payload:
        return ""
    try:
        text = payload.rstrip(b"\x00").decode("ascii")
    except UnicodeDecodeError:
        return ""
    if "<Message" in text or "<?xml" in text:
        return text
    return ""


def parse_frames(blob, direction):
    frames = []
    i = 0
    while i + 32 <= len(blob):
        if blob[i:i + 4] != MAGIC:
            next_magic = blob.find(MAGIC, i + 1)
            if next_magic < 0:
                break
            i = next_magic
            continue
        hdr = blob[i:i + 32]
        cmd = struct.unpack("<I", hdr[4:8])[0]
        seq = struct.unpack("<I", hdr[8:12])[0]
        flag = struct.unpack("<I", hdr[12:16])[0]
        session = struct.unpack("<I", hdr[16:20])[0]
        extra = struct.unpack("<I", hdr[20:24])[0]
        length = struct.unpack("<I", hdr[28:32])[0]
        end = i + 32 + length
        if end > len(blob):
            break
        payload = blob[i + 32:end]
        frames.append(Frame(direction, cmd, seq, flag, session, extra, length, payload))
        i = end
    return frames


def summarize_frame(idx, frame):
    base = (
        f"{idx:03d} {frame.direction:>14} "
        f"cmd=0x{frame.cmd:04x} seq={frame.seq} "
        f"flag=0x{frame.flag:08x} session=0x{frame.session:08x} "
        f"extra=0x{frame.extra:08x} len={frame.length}"
    )
    xml = try_ascii(frame.payload)
    if xml:
        one_line = " ".join(x.strip() for x in xml.splitlines() if x.strip())
        return base + "\n    " + one_line
    if frame.length == 4:
        value = struct.unpack("<I", frame.payload)[0]
        return base + f"\n    u32={value}"

    # Check for H.264 start code and SPS/PPS (payload uses a 12-byte prefix)
    if frame.length > 16:  # 12 byte prefix + 4 byte start code
        h264_payload = frame.payload[12:]
        if h264_payload.startswith(b"\x00\x00\x00\x01"):
            nal_type = h264_payload[4] & 0x1F
            if nal_type == 7:  # SPS
                return base + f"\n    h264/annex-b SPS payload:\n    hex={frame.payload.hex()}"
            if nal_type == 8:  # PPS
                return base + f"\n    h264/annex-b PPS payload:\n    hex={frame.payload.hex()}"
            if nal_type == 5:  # IDR
                return base + f"\n    h264/annex-b IDR (I-frame) payload:\n    hex={frame.payload.hex()}"
            return base + "\n    h264/annex-b payload"
    if frame.payload.startswith(b"\x00\x00\x00\x01"):
        return base + "\n    h264/annex-b payload"
    if frame.length:
        preview = frame.payload[:24].hex()
        return base + f"\n    hex={preview}{'...' if frame.length > 24 else ''}"
    return base


def analyze(pcap, port):
    streams = list_streams(pcap, port)
    print(f"port {port}: tcp streams {streams}")
    for stream_id in streams:
        print(f"\n== stream {stream_id} ==")
        chunks = follow_stream_hex(pcap, stream_id)
        frames = []
        for direction, blob in chunks:
            frames.extend(parse_frames(blob, direction))
        for idx, frame in enumerate(frames, 1):
            print(summarize_frame(idx, frame))


def main():
    parser = argparse.ArgumentParser(description="Decode SmartMEye (DVRIP) protocol frames from a pcapng.")
    parser.add_argument("pcap", help="Path to pcap/pcapng file")
    parser.add_argument("--port", type=int, action="append", default=[], help="Port to analyze")
    args = parser.parse_args()

    ports = args.port or [6001, 6002]
    for port in ports:
        analyze(args.pcap, port)


if __name__ == "__main__":
    main()
