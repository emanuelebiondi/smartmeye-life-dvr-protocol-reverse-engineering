#!/usr/bin/env python3
"""
Probe helper for legacy DVR playback protocol.

Goals:
- Discover which XML playback/query commands are accepted by the DVR.
- Try a best-effort playback start and dump Annex-B H264 to file.

This is intentionally diagnostic: it does not replace legacybridge live mode.
"""

from __future__ import annotations

import argparse
import dataclasses
import socket
import struct
import time
from typing import Iterable, Optional


MAGIC = b"\x5a\x5a\xaa\x55"
HEADER_LEN = 32
MEDIA_HEADER_LEN = 44


@dataclasses.dataclass(frozen=True)
class Family:
    name: str
    login_cmd: int
    bootstrap: list[tuple[int, int, int]]
    stream_base: int


LEGACY = Family(
    name="legacy",
    login_cmd=0x56F5,
    bootstrap=[
        (0x56F6, 1100, 0x00),
        (0x56F7, 1108, 0x01),
        (0x56F8, 1201, 0x00),
        (0x56F9, 1100, 0x00),
        (0x56FA, 1024, 0xFF),
        (0x56FB, 1010, 0x01),
        (0x56FC, 1010, 0x02),
        (0x56FD, 1010, 0x04),
        (0x56FE, 1010, 0x08),
        (0x56FF, 1010, 0x10),
        (0x5700, 1010, 0x20),
        (0x5701, 1010, 0x40),
        (0x5702, 1010, 0x80),
    ],
    stream_base=0x5703,
)

NEW = Family(
    name="new",
    login_cmd=0x606D,
    bootstrap=[
        (0x606E, 1100, 0x00),
        (0x606F, 1108, 0x01),
        (0x6070, 1201, 0x00),
        (0x6071, 1100, 0x00),
        (0x6072, 1024, 0xFF),
        (0x6073, 1010, 0x01),
        (0x6074, 1010, 0x02),
        (0x6075, 1010, 0x04),
    ],
    stream_base=0x6076,
)


def build_frame(cmd: int, seq: int, flag: int, session: int, extra: int, payload: bytes = b"") -> bytes:
    return (
        MAGIC
        + struct.pack("<I", cmd)
        + struct.pack("<I", seq)
        + struct.pack("<I", flag)
        + struct.pack("<I", session)
        + struct.pack("<I", extra)
        + b"\x00\x00\x00\x00"
        + struct.pack("<I", len(payload))
        + payload
    )


def recv_exact(sock: socket.socket, n: int) -> bytes:
    out = bytearray()
    while len(out) < n:
        chunk = sock.recv(n - len(out))
        if not chunk:
            raise ConnectionError("socket closed")
        out.extend(chunk)
    return bytes(out)


def read_frame(sock: socket.socket, timeout: float = 1.0) -> tuple[int, int, int, int, int, bytes]:
    sock.settimeout(timeout)
    hdr = recv_exact(sock, HEADER_LEN)
    if hdr[:4] != MAGIC:
        raise ValueError(f"bad magic: {hdr[:4].hex()}")
    cmd, seq, flag, session, extra, _rsv, plen = struct.unpack("<IIIIIII", hdr[4:32])
    payload = recv_exact(sock, plen) if plen else b""
    return cmd, seq, flag, session, extra, payload


def make_xml(inner: str) -> bytes:
    xml = (
        '<?xml version="1.0" ?>\n'
        "<Message Version=\"1\">\n"
        "    <Header>\n"
        f"        {inner}\n"
        "    </Header>\n"
        "</Message>\n"
    )
    return (xml + "\x00").encode("ascii")


def compact(payload: bytes) -> str:
    txt = payload.decode("latin1", errors="ignore").replace("\x00", "")
    return " ".join(txt.split())


def extract_tags(payload: bytes) -> list[str]:
    txt = payload.decode("latin1", errors="ignore")
    out = []
    idx = 0
    while True:
        i = txt.find("<", idx)
        if i < 0:
            return out
        j = i + 1
        while j < len(txt) and (txt[j].isalnum() or txt[j] == "_"):
            j += 1
        if j > i + 1:
            out.append(txt[i + 1 : j])
        idx = j


def annexb_start(payload: bytes) -> int:
    i = payload.find(b"\x00\x00\x00\x01")
    if i >= 0 and i + 4 < len(payload):
        return i
    i = payload.find(b"\x00\x00\x01")
    if i >= 0 and i + 3 < len(payload):
        return i
    return -1


class ProbeClient:
    def __init__(self, host: str, cmd_port: int, data_port: int, user: str, password: str, verbose: bool = True):
        self.host = host
        self.cmd_port = cmd_port
        self.data_port = data_port
        self.user = user
        self.password = password
        self.verbose = verbose
        self.cmd: Optional[socket.socket] = None
        self.data: Optional[socket.socket] = None
        self.family: Optional[Family] = None

    def log(self, *parts: object) -> None:
        if self.verbose:
            print(*parts)

    def connect_cmd(self) -> None:
        self.cmd = socket.create_connection((self.host, self.cmd_port), timeout=5)

    def connect_data(self) -> None:
        self.data = socket.create_connection((self.host, self.data_port), timeout=5)

    def close(self) -> None:
        if self.cmd is not None:
            self.cmd.close()
            self.cmd = None
        if self.data is not None:
            self.data.close()
            self.data = None

    def send_cmd(self, cmd: int, seq: int, flag: int, session: int, extra: int, payload: bytes = b"") -> None:
        assert self.cmd is not None
        self.cmd.sendall(build_frame(cmd, seq, flag, session, extra, payload))
        self.log(f"cmd-> 0x{cmd:04x} seq={seq} extra=0x{extra:x} len={len(payload)}")

    def send_xml(self, cmd: int, seq: int, extra: int, inner_xml: str) -> None:
        self.send_cmd(cmd, seq, 0, 0, extra, make_xml(inner_xml))

    def login_auto(self, family_hint: str) -> Family:
        assert self.cmd is not None
        families: list[Family]
        if family_hint == "legacy":
            families = [LEGACY]
        elif family_hint == "new":
            families = [NEW]
        else:
            families = [NEW, LEGACY]

        payload = make_xml(f'<login_request username="{self.user}" password="{self.password}" />')
        for fam in families:
            self.send_cmd(fam.login_cmd, 100, 0, 0xFFFFFFFF, 0, payload)
            deadline = time.time() + 2.0
            while time.time() < deadline:
                try:
                    cmd, seq, _flag, _sess, _extra, rep = read_frame(self.cmd, timeout=0.3)
                except Exception:
                    break
                if cmd == fam.login_cmd:
                    self.log(f"login ok with family={fam.name}, reply seq={seq}, len={len(rep)}")
                    self.family = fam
                    return fam
            self.log(f"login retry with next family (failed: {fam.name})")

        raise RuntimeError("login failed with all families")

    def bootstrap(self) -> None:
        assert self.family is not None
        for cmd, seq, extra in self.family.bootstrap:
            self.send_cmd(cmd, seq, 0, 0, extra)
            time.sleep(0.03)
        # Keepalive burst helps both families.
        for _ in range(4):
            self.send_cmd(0x0000, 801, 1, 0, 0)
            time.sleep(0.03)

    def drain_cmd(self, seconds: float = 0.8) -> None:
        assert self.cmd is not None
        end = time.time() + seconds
        while time.time() < end:
            try:
                cmd, seq, _flag, _sess, _extra, payload = read_frame(self.cmd, timeout=0.05)
            except Exception:
                continue
            if payload:
                self.log(f"cmd<- 0x{cmd:04x} seq={seq} len={len(payload)} {compact(payload)[:140]}")

    def base_for_channel(self, channel: int, channel_base: int) -> tuple[int, int]:
        assert self.family is not None
        proto = channel - channel_base
        if proto < 0:
            raise ValueError("channel-base mapping is negative")
        return self.family.stream_base + proto * 3, proto

    def open_stream_and_get_socket(self, channel: int, stream: int, channel_base: int) -> int:
        """
        Live-style open sequence used as a transport bootstrap for playback probes.
        """
        assert self.data is not None
        base, proto = self.base_for_channel(channel, channel_base)
        extra = 1 << proto

        self.send_cmd(base, 1108, 0, 0, extra)
        self.send_xml(base, 1201, extra, f'<stream_ch_request channel="{proto}" stream="{stream}" />')

        cmd, seq, _flag, _session, _extra, payload = read_frame(self.data, timeout=4.0)
        if len(payload) != 4:
            raise RuntimeError(f"unexpected first media frame cmd=0x{cmd:04x} seq={seq} len={len(payload)}")
        socket_id = struct.unpack("<I", payload)[0]
        self.log(f"media socket_id={socket_id} cmd=0x{cmd:04x} seq={seq}")

        # ACK socket id on data channel.
        assert self.data is not None
        self.data.sendall(build_frame(0, 800, 0, 0, 0, payload))
        return socket_id

    def playback_start_requests(
        self,
        channel: int,
        stream: int,
        channel_base: int,
        socket_id: int,
        start_time: str,
        stop_time: str,
    ) -> None:
        base, proto = self.base_for_channel(channel, channel_base)
        extra = 1 << proto
        cmd = base + 1
        variants = [
            f'<start_playback_request socket="{socket_id}" channel="{proto}" stream="{stream}" start_time="{start_time}" stop_time="{stop_time}" />',
            f'<start_playback_request channel="{proto}" stream="{stream}" start_time="{start_time}" stop_time="{stop_time}" />',
            f'<start_playback_request socket="{socket_id}" start_time="{start_time}" stop_time="{stop_time}" />',
        ]
        for i, xml in enumerate(variants, start=1):
            self.send_xml(cmd, 200, extra, xml)
            self.log(f"sent playback variant #{i}")
            time.sleep(0.06)

        data_cmd = base + 2
        data_variants = [
            f'<playback_data_request socket="{socket_id}" param="1" />',
            '<playback_data_request param="1" />',
        ]
        for xml in data_variants:
            self.send_xml(data_cmd, 1303, extra, xml)
            time.sleep(0.06)

    def scan_queries(self, channel: int, channel_base: int, day: str) -> None:
        base, proto = self.base_for_channel(channel, channel_base)
        extra = 1 << proto
        cmd = base + 9  # keep separate from live/open commands

        queries = [
            '<get_record_month_request />',
            f'<get_record_day_info channel="{proto}" />',
            f'<get_record_day_info channel="{proto}" info="{day}" />',
            f'<start_find_file_request channel="{proto}" start_time="{day} 00:00:00" stop_time="{day} 23:59:59" main_type="0" sub_type="0" file_type="0" />',
            '<find_result_request entry_cnt="128" buf_len="32768" />',
        ]
        seqs = [200, 1201]
        ignore = {
            "Message",
            "Header",
            "server_event",
            "dev_info",
            "ch_attri_cfg",
            "cfg1",
            "ch_basic_cfg",
            "color",
            "vl",
            "motion",
            "mosaic",
            "stream_ch",
        }

        assert self.cmd is not None
        for q in queries:
            for seq in seqs:
                self.send_xml(cmd, seq, extra, q)
                hit = False
                end = time.time() + 0.8
                while time.time() < end:
                    try:
                        rcmd, rseq, _flag, _sess, rextra, payload = read_frame(self.cmd, timeout=0.1)
                    except Exception:
                        continue
                    if not payload:
                        continue
                    tags = extract_tags(payload)
                    extra_tags = [t for t in tags if t not in ignore]
                    if extra_tags:
                        self.log(
                            f"query-hit send_cmd=0x{cmd:04x} send_seq={seq} recv_cmd=0x{rcmd:04x} recv_seq={rseq} recv_extra=0x{rextra:x}"
                        )
                        self.log(f"tags={extra_tags[:8]} payload={compact(payload)[:240]}")
                        hit = True
                        break
                if not hit:
                    self.log(f"query-nohit cmd=0x{cmd:04x} seq={seq} xml={q}")
                cmd += 1

    def dump_playback_media(self, seconds: float, out_h264: str, channel: int, channel_base: int) -> int:
        assert self.data is not None
        proto = channel - channel_base
        pending = bytearray()
        out = open(out_h264, "wb")
        written = 0
        end = time.time() + seconds
        try:
            while time.time() < end:
                self.data.settimeout(0.6)
                try:
                    chunk = self.data.recv(65535)
                except socket.timeout:
                    continue
                if not chunk:
                    break
                pending.extend(chunk)

                i = 0
                while i + MEDIA_HEADER_LEN <= len(pending):
                    if pending[i : i + 4] != MAGIC:
                        j = pending.find(MAGIC, i + 1)
                        if j < 0:
                            break
                        i = j
                        continue
                    payload_len = struct.unpack("<I", pending[i + 40 : i + 44])[0]
                    if payload_len > 8 * 1024 * 1024:
                        i += 1
                        continue
                    frame_len = MEDIA_HEADER_LEN + payload_len
                    if i + frame_len > len(pending):
                        break
                    frame = pending[i : i + frame_len]
                    cmd = struct.unpack("<I", frame[4:8])[0]
                    frame_type = struct.unpack("<I", frame[8:12])[0]
                    payload = frame[MEDIA_HEADER_LEN:]

                    # Keep DVR in sync.
                    self.data.sendall(build_frame(cmd, 2, 2, 0x64, 0))

                    if cmd == proto and frame_type in (0, 1) and payload:
                        off = annexb_start(payload)
                        if off >= 0:
                            chunk_out = payload[off:]
                            out.write(chunk_out)
                            written += len(chunk_out)
                    i += frame_len

                if i > 0:
                    del pending[:i]
        finally:
            out.close()
        return written


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Legacy DVR playback/query probe")
    p.add_argument("--host", default="192.168.1.10")
    p.add_argument("--cmd-port", type=int, default=6001)
    p.add_argument("--data-port", type=int, default=6002)
    p.add_argument("--user", default="Admin")
    p.add_argument("--password", default="")
    p.add_argument("--family", choices=["auto", "legacy", "new"], default="auto")
    p.add_argument("--channel", type=int, default=1, help="User-facing channel number")
    p.add_argument("--channel-base", type=int, default=1, help="Set 0 if your channels are 0..N")
    p.add_argument("--stream", type=int, default=0)
    p.add_argument("--quiet", action="store_true")

    sub = p.add_subparsers(dest="mode", required=True)

    scan = sub.add_parser("scan", help="Probe record query commands")
    scan.add_argument("--day", default=time.strftime("%Y-%m-%d"))

    play = sub.add_parser("play", help="Best-effort playback start + media dump")
    play.add_argument("--start-time", required=True, help='Format: "YYYY-MM-DD HH:MM:SS"')
    play.add_argument("--stop-time", required=True, help='Format: "YYYY-MM-DD HH:MM:SS"')
    play.add_argument("--seconds", type=float, default=20.0, help="Capture window on media socket")
    play.add_argument("--out", default="playback_probe.h264")

    return p.parse_args()


def main() -> int:
    args = parse_args()
    cl = ProbeClient(
        host=args.host,
        cmd_port=args.cmd_port,
        data_port=args.data_port,
        user=args.user,
        password=args.password,
        verbose=not args.quiet,
    )
    try:
        cl.connect_cmd()
        cl.login_auto(args.family)
        cl.bootstrap()
        cl.drain_cmd(0.6)

        if args.mode == "scan":
            cl.scan_queries(channel=args.channel, channel_base=args.channel_base, day=args.day)
            return 0

        if args.mode == "play":
            cl.connect_data()
            socket_id = cl.open_stream_and_get_socket(
                channel=args.channel, stream=args.stream, channel_base=args.channel_base
            )
            cl.playback_start_requests(
                channel=args.channel,
                stream=args.stream,
                channel_base=args.channel_base,
                socket_id=socket_id,
                start_time=args.start_time,
                stop_time=args.stop_time,
            )
            cl.drain_cmd(1.0)
            written = cl.dump_playback_media(
                seconds=args.seconds,
                out_h264=args.out,
                channel=args.channel,
                channel_base=args.channel_base,
            )
            print(f"media dump completed: bytes={written} file={args.out}")
            if written == 0:
                print("no H264 payload extracted; likely XML/cmd variant still not correct for playback.")
            return 0

        raise RuntimeError(f"unsupported mode: {args.mode}")
    finally:
        cl.close()


if __name__ == "__main__":
    raise SystemExit(main())
