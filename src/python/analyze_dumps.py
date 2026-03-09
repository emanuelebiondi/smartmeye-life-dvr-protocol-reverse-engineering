#!/usr/bin/env python3
"""
Analizza uno o più dump Wireshark (pcap/pcapng) con il protocollo DVRIP (SmartMEye).
Uso: python3 analyze_dumps.py dump1.pcapng [dump2.pcapng ...] [--port 6001] [--port 6002]
"""
import argparse
import subprocess
import sys
from pathlib import Path

# Importa dalla root del repo
REPO_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO_ROOT))

from analyze_smartmeyi_pcap import analyze  # noqa: E402


def main():
    parser = argparse.ArgumentParser(
        description="Analizza più dump Wireshark (protocollo DVRIP/SmartMEye, porte 6001/6002)."
    )
    parser.add_argument(
        "pcaps",
        nargs="+",
        help="File pcap o pcapng da analizzare",
    )
    parser.add_argument(
        "--port",
        type=int,
        action="append",
        default=[],
        help="Porta da analizzare (default: 6001 6002)",
    )
    args = parser.parse_args()

    ports = args.port or [6001, 6002]

    for pcap_path in args.pcaps:
        p = Path(pcap_path)
        if not p.exists():
            print(f"File non trovato: {pcap_path}", file=sys.stderr)
            continue
        print("=" * 60)
        print(f"  {p.name}")
        print("=" * 60)
        for port in ports:
            try:
                analyze(str(p), port)
            except Exception as e:
                print(f"Errore su {p.name} porta {port}: {e}", file=sys.stderr)
        print()


if __name__ == "__main__":
    main()
