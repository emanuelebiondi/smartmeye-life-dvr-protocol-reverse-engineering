#!/usr/bin/env python3
import argparse
import struct
from pathlib import Path

MAGIC = b"\x5a\x5a\xaa\x55"
MEDIA_PREFIX_LEN = 12


def extract_h264(src: bytes) -> bytes:
    out = bytearray()
    i = 0
    while i + 32 <= len(src):
        if src[i:i + 4] != MAGIC:
            j = src.find(MAGIC, i + 1)
            if j < 0:
                break
            i = j
            continue

        payload_len = struct.unpack("<I", src[i + 28:i + 32])[0]
        end = i + 32 + payload_len
        if end > len(src):
            break

        payload = src[i + 32:end]
        if payload_len > MEDIA_PREFIX_LEN:
            out.extend(payload[MEDIA_PREFIX_LEN:])
        i = end

    return bytes(out)


def main():
    parser = argparse.ArgumentParser(description="Extract H264 from a raw port-6002 dump.")
    parser.add_argument("input", help="Raw proprietary dump")
    parser.add_argument("-o", "--output", default="output.h264", help="Output H264 file")
    args = parser.parse_args()

    src = Path(args.input).read_bytes()
    h264 = extract_h264(src)
    Path(args.output).write_bytes(h264)
    print(f"wrote {len(h264)} bytes to {args.output}")


if __name__ == "__main__":
    main()
