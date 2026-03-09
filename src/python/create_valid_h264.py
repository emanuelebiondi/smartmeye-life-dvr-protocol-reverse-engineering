#!/usr/bin/env python3
import argparse
import binascii
import re
import struct
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

MAGIC = b"\x5a\x5a\xaa\x55"
MEDIA_PREFIX_LEN = 12


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
        print("tshark non trovato nel PATH", file=sys.stderr)
        sys.exit(1)


def list_streams(pcap, port):
    out = run_tshark(["-r", pcap, "-T", "fields", "-e", "tcp.stream", "-Y", f"tcp.port=={port}"])
    streams = sorted({int(x) for x in out.split() if x.strip().isdigit()})
    return streams


def follow_stream_hex(pcap, stream_id):
    out = run_tshark(["-r", pcap, "-q", "-z", f"follow,tcp,raw,{stream_id}"])
    chunks = []
    # Server -> client data starts with a tab. Client -> server does not.
    # We are only interested in server -> client data for the video stream.
    for line in out.splitlines():
        if line.startswith("	"):
            data = binascii.unhexlify(line.strip())
            chunks.append(data)
    return b"".join(chunks)


def parse_frames(blob):
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
        frames.append(Frame("server->client", cmd, seq, flag, session, extra, length, payload))
        i = end
    return frames


def create_valid_h264_stream(frames, channel):
    out = bytearray()
    config_data = None

    # First, find a global configuration packet (SPS/PPS/IDR)
    # These seem to have seq = 0
    for frame in frames:
        if frame.seq == 0 and frame.length > MEDIA_PREFIX_LEN:
            h264_payload = frame.payload[MEDIA_PREFIX_LEN:]
            if h264_payload.startswith(b"\x00\x00\x00\x01"):
                nal_type = h264_payload[4] & 0x1F
                if nal_type == 7: # SPS
                    print(f"Trovato pacchetto di configurazione (SPS/PPS/IDR) nel frame con cmd={frame.cmd}")
                    config_data = h264_payload
                    break # Stop after finding the first one

    if not config_data:
        return None # Indicate failure

    out.extend(config_data)

    # Now, append all video frames for the specified channel
    for frame in frames:
        if frame.cmd == channel and frame.seq == 1 and frame.length > MEDIA_PREFIX_LEN:
            h264_payload = frame.payload[MEDIA_PREFIX_LEN:]
            if h264_payload.startswith(b"\x00\x00\x00\x01"):
                 out.extend(h264_payload)

    return bytes(out)


def main():
    parser = argparse.ArgumentParser(description="Estrae un flusso H264 valido da un pcap del DVR.")
    parser.add_argument("pcap", help="Path del file pcap/pcapng")
    parser.add_argument("--port", type=int, default=6002, help="Porta dati del DVR")
    parser.add_argument("--channel", type=int, default=5, help="Canale video da estrarre")
    parser.add_argument("-o", "--output", default="video_corretto.h264", help="File H264 in uscita")
    args = parser.parse_args()

    streams = list_streams(args.pcap, args.port)
    if not streams:
        print(f"Nessuno stream TCP trovato sulla porta {args.port}", file=sys.stderr)
        sys.exit(1)

    print(f"Trovato stream TCP {streams[0]} sulla porta {args.port}")

    # We assume the first stream is the correct one
    stream_data = follow_stream_hex(args.pcap, streams[0])
    frames = parse_frames(stream_data)

    print(f"Analizzati {len(frames)} frame dal protocollo proprietario.")

    h264_stream = create_valid_h264_stream(frames, args.channel)

    if not h264_stream:
        print("Non è stato possibile estrarre un flusso H.264. Nessun pacchetto di configurazione trovato.", file=sys.stderr)
        sys.exit(1)

    Path(args.output).write_bytes(h264_stream)
    print(f"Flusso H.264 valido scritto su {args.output} ({len(h264_stream)} byte)")
    print("\nProva a riprodurlo con:\nffplay video_corretto.h264")


if __name__ == "__main__":
    main()
