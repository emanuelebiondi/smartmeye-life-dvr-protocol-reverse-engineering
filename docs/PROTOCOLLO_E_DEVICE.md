# Protocollo e dispositivo DVR

## Marca e modello: Life D/N/I 2013

- **Life Electronics S.p.A.** (Italia): produttore di sistemi di videosorveglianza, DVR/NVR, telecamere.
  - Sito: [lifeshop.it](https://www.lifeshop.it), [lifevideocontrollo.it](http://www.lifevideocontrollo.it).
- Il modello **D/N/I 2013** si connette con l’**app SmartMEye** (non XMEye: quella non funziona con questo DVR). Il protocollo è **DVRIP/Sofia** (stesso stack di Xiongmai/NETsurveillance): magic `5a 5a aa 55`, porte **6001** (comandi) e **6002** (stream media).
- Riferimenti protocollo: [DVRIP library (GitHub)](https://github.com/alexshpilkin/dvrip), [python-dvr](https://github.com/xyyangkun/python-dvr).

---

## Struttura del protocollo (da analisi dump)

### Frame generico (TCP)

Ogni messaggio inizia con un header di **32 byte**:

| Offset | Lunghezza | Contenuto |
|--------|-----------|-----------|
| 0   | 4 | Magic `5a 5a aa 55` |
| 4   | 4 | `cmd` (LE) – comando/canale |
| 8   | 4 | `seq` (LE) – sequenza/tipo (0, 1, 2, 800, 801, …) |
| 12  | 4 | `flag` (LE) |
| 16  | 4 | `session` (LE) |
| 20  | 4 | `extra` (LE) |
| 24  | 4 | riservato (0) |
| 28  | 4 | `payload_len` (LE) |
| 32  | N | payload (N = payload_len) |

### Porta 6001 (comandi)

- **Login**: client invia `cmd=0x56F5`, XML con `login_request` (username, password). Server risponde con `login_reply` e `data_port="6002"`.
- **Bootstrap**: sequenza di comandi senza payload (0x56F6–0x5702) e keepalive (cmd=0, seq=801).
- **Device info**: server invia `dev_info` (board_name tipo `jiuan_his3520d_v300_tp2823_8`, 8 canali analogici, stream 704×576, ecc.).
- **Stream/channel**: `stream_ch`, `open_channel_request`, `open_audio_request` con XML; `cmd` in base al canale (0x5703 + offset per canale).
- **Eventi**: server invia `server_event` (channel, main, sub).

### Porta 6002 (media)

- **Primo messaggio**: server invia `cmd=0`, `seq=800`, `len=4` → payload = **socket_id** (LE). Il client deve rispondere con **ACK**: stesso `cmd=0`, `seq=800`, payload = socket_id (come in `main.py` e `legacybridge`).
- **Stream video**: frame con `cmd` = numero canale (0, 1, 2, …), `seq=1` per dati, `len` = lunghezza payload.
- **Payload video**: dopo l’header 32 byte c’è un **prefisso di 12 byte** (dipende dal firmware), poi **H.264 Annex-B** (startcode `00 00 00 01` o `00 00 01` + NAL). In alcuni dump si vede anche `seq=2`, `session=0x64`, `len=0` come ACK client→server; senza questi ACK il DVR va fuori sync (video grigio).
- **Riepilogo (da analisi dump2/dump3)**: **ogni** pacchetto (seq=1 e seq=2 con `len`>0) ha prefisso 12 byte → usare sempre `payload[12:]` = H.264. I pacchetti seq=2 con payload sono continuazione NAL (stesso prefisso 12 byte, niente startcode). In legacybridge il default è ora “prefisso 12” per le continuazioni; usare `--continuation-no-prefix` solo se il DVR invia continuazioni senza prefisso.

---

## Come analizzare i tre dump Wireshark

1. Metti i file pcap/pcapng nella cartella del progetto (es. `dump1.pcapng`, `dump2.pcapng`, `dump3.pcapng`).
2. Richiesto **tshark** (Wireshark da riga di comando):
   ```bash
   # Analisi porte 6001 e 6002 su tutti i dump
   python3 analyze_smartmeyi_pcap.py dump1.pcapng
   python3 analyze_smartmeyi_pcap.py dump2.pcapng --port 6001 --port 6002
   python3 analyze_smartmeyi_pcap.py dump3.pcapng
   ```
3. Per analizzare più dump in un colpo solo:
   ```bash
   python3 scripts/analyze_dumps.py dump1.pcapng dump2.pcapng dump3.pcapng
   ```
4. Per estrarre H.264 da uno stream della porta 6002 (es. canale 5):
   ```bash
   python3 create_valid_h264.py dump1.pcapng --port 6002 --channel 5 -o canale5.h264
   ffplay canale5.h264
   ```

---

## File di progetto rilevanti

| File | Uso |
|------|-----|
| `analyze_smartmeyi_pcap.py` | Analisi frame per porta (6001/6002), decodifica XML e hint H.264 |
| `create_valid_h264.py` | Estrazione H.264 da un pcap (porta 6002, un canale) |
| `extract_h264.py` | Estrazione da dump **raw** (binario già estratto dalla 6002), sempre `payload[12:]` |
| `main.py` | Script Python live: handshake + stream, `payload[12:]` + sync su startcode, ACK media |
| `legacybridge/` | Bridge Go per go2rtc, stessi comandi/ACK, opzioni per prefisso e continuazione |

I log di analisi già presenti (`analysis_output.txt`, `log6001.txt`, `log6002.txt`) sono stati generati da `analyze_smartmeyi_pcap.py` su un singolo dump; per “i tre dump” ripetere l’analisi sui tre file pcap quando disponibili.
