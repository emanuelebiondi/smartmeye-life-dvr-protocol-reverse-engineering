#!/usr/bin/env python3
import argparse
import binascii
import re
import struct
import subprocess
from collections import Counter, defaultdict

MAGIC = b"\x5a\x5a\xaa\x55"


def run_tshark(args):
    return subprocess.check_output(["tshark", *args], text=True, errors="ignore")


def list_streams(pcap, port):
    out = run_tshark(["-r", pcap, "-T", "fields", "-e", "tcp.stream", "-Y", f"tcp.port=={port}"])
    return sorted({int(x) for x in out.split() if x.isdigit()})


def follow_stream_hex(pcap, stream_id):
    out = run_tshark(["-r", pcap, "-q", "-z", f"follow,tcp,raw,{stream_id}"])
    chunks = []
    for line in out.splitlines():
        if re.fullmatch(r"\t?[0-9a-f]+", line):
            direction = "s2c" if line.startswith("\t") else "c2s"
            chunks.append((direction, binascii.unhexlify(line.strip())))
    return chunks


def parse_frames(blob, direction):
    frames = []
    i = 0
    while i + 32 <= len(blob):
        if blob[i:i + 4] != MAGIC:
            j = blob.find(MAGIC, i + 1)
            if j < 0:
                break
            i = j
            continue
        hdr = blob[i : i + 32]
        cmd = struct.unpack("<I", hdr[4:8])[0]
        seq = struct.unpack("<I", hdr[8:12])[0]
        flag = struct.unpack("<I", hdr[12:16])[0]
        session = struct.unpack("<I", hdr[16:20])[0]
        extra = struct.unpack("<I", hdr[20:24])[0]
        length = struct.unpack("<I", hdr[28:32])[0]
        end = i + 32 + length
        if end > len(blob):
            break
        payload = blob[i + 32 : end]
        frames.append((direction, cmd, seq, flag, session, extra, length, payload))
        i = end
    return frames


def summarize_6002(frames):
    s2c = [f for f in frames if f[0] == "s2c"]
    by_cmd = defaultdict(Counter)
    h264_hits = Counter()
    for _, cmd, seq, _, _, _, length, payload in s2c:
        by_cmd[cmd][seq] += 1
        if length > 12 and payload[12:16] == b"\x00\x00\x00\x01":
            h264_hits[cmd] += 1
    return by_cmd, h264_hits


def main():
    parser = argparse.ArgumentParser(description="Confronta dump SmartMEye e mostra multiplex canali su 6002.")
    parser.add_argument("pcaps", nargs="+", help="Percorsi pcap/pcapng")
    args = parser.parse_args()

    for pcap in args.pcaps:
        print(f"\n=== {pcap} ===")
        for port in (6001, 6002, 6003):
            streams = list_streams(pcap, port)
            print(f"porta {port}: stream {streams}")
            if not streams:
                continue

            all_frames = []
            for sid in streams:
                for direction, blob in follow_stream_hex(pcap, sid):
                    all_frames.extend(parse_frames(blob, direction))

            if port == 6002:
                by_cmd, h264_hits = summarize_6002(all_frames)
                print("  media cmd -> seq count:")
                for cmd in sorted(by_cmd):
                    seq_summary = ", ".join(f"seq{seq}:{count}" for seq, count in sorted(by_cmd[cmd].items()))
                    print(f"    cmd {cmd}: {seq_summary}; h264@+12={h264_hits.get(cmd, 0)}")
                if len(by_cmd) > 1:
                    print("  nota: flusso MULTIPLEX su 6002 (piu canali insieme)")
                else:
                    print("  nota: flusso singolo canale su 6002")


if __name__ == "__main__":
    main()
