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
    parser = argparse.ArgumentParser(description="Estrae H264 da un dump raw della porta 6002.")
    parser.add_argument("input", help="Dump raw proprietario")
    parser.add_argument("-o", "--output", default="output.h264", help="File H264 in uscita")
    args = parser.parse_args()

    src = Path(args.input).read_bytes()
    h264 = extract_h264(src)
    Path(args.output).write_bytes(h264)
    print(f"scritti {len(h264)} byte su {args.output}")


if __name__ == "__main__":
    main()
