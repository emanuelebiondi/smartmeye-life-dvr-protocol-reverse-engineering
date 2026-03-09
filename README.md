# Replica protocollo DVR Life / SmartMEye

Bridge e script per estrarre video H.264 da un DVR che usa il protocollo **DVRIP** con l’**app SmartMEye** (non XMEye: non funziona con questo DVR). Magic `5a 5a aa 55`, porte 6001 comandi / 6002 media.

- **Dispositivo**: Life D/N/I 2013 (Life Electronics S.p.A.). **App**: SmartMEye.
- **Documentazione protocollo e analisi dump**: [docs/PROTOCOLLO_E_DEVICE.md](docs/PROTOCOLLO_E_DEVICE.md).

## Analisi dei dump Wireshark

Con **tshark** installato, per analizzare uno o più pcap/pcapng (es. i “tre dump”):

```bash
# Un solo dump
python3 src/python/analyze_smartmeyi_pcap.py captures/mio_dump.pcapng

# Più dump in una volta (porte 6001 e 6002)
python3 src/python/analyze_dumps.py captures/dump1.pcapng captures/dump2.pcapng captures/dump3.pcapng
```

## Bridge Go (go2rtc)

Vedi [legacybridge/README.md](legacybridge/README.md) per build, uso live e opzioni (`--all-12`, `--continuation-skip`, ecc.).

## Operativo rapido (Docker)

```bash
cd docker
docker compose up -d --build
./diag.sh
```

Profili stream disponibili in go2rtc:

- `dvr_cam1..dvr_cam5` (default da `DVR_STREAM`)
- `dvr_camX_main` (qualita')
- `dvr_camX_sub` (latenza bassa)
- `dvr_camX_auto` (tentativo sub con fallback)

## Struttura progetto

- `legacybridge/`: bridge Go legacy -> H264 stdout per go2rtc
- `docker/`: compose + config go2rtc + Dockerfile
- `src/python/`: script Python di analisi, estrazione e test protocollo
- `docs/`: note tecniche e reverse engineering

`captures/` e `artifacts/` non sono inclusi nel repo pubblico (restano locali, fuori git).

## Script principali

| Script | Uso |
|--------|-----|
| `src/python/analyze_smartmeyi_pcap.py` | Analisi frame per porta (6001/6002) |
| `src/python/analyze_dumps.py` | Analisi multipla di più pcap |
| `src/python/analyze_new_captures.py` | Confronto dump NEW e rilevazione multiplex canali su 6002 |
| `src/python/create_valid_h264.py` | Estrazione H.264 da un pcap (porta 6002) |
| `src/python/playback_probe.py` | Probe playback/query (get_record_*, find_file, start_playback) |

## Playback (stato attuale)

Il playback non e' ancora stabilizzato nel bridge Go, ma ora c'e' un probe dedicato:

```bash
# 1) Verifica se il DVR risponde ai comandi record/query
python3 src/python/playback_probe.py \
  --host 192.168.1.10 --user Admin --password 'PASSWORD' \
  --channel 1 --channel-base 1 \
  scan --day 2026-03-09

# 2) Tentativo best-effort di start playback + dump H264
python3 src/python/playback_probe.py \
  --host 192.168.1.10 --user Admin --password 'PASSWORD' \
  --channel 1 --channel-base 1 \
  play \
  --start-time '2026-03-09 00:00:00' \
  --stop-time '2026-03-09 00:10:00' \
  --seconds 20 \
  --out playback_probe.h264
```

Se il file resta a 0 byte, significa che il DVR non ha accettato (o non ha capito) la variante XML inviata: in quel caso serve una capture Wireshark fatta durante una vera sessione "Remote Playback" da client originale.
