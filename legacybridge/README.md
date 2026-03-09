# Legacy DVR bridge for go2rtc

Bridge minimale per DVR legacy **SmartMEye** (Life D/N/I 2013, protocollo DVRIP):

- apre `6001/6002`
- esegue l'handshake legacy
- estrae H264 dal flusso proprietario della `6002`
- scrive il bitstream su `stdout`

Questo formato e' adatto a una sorgente `exec` di `go2rtc`.

## Build

```bash
cd legacybridge
go build -o legacybridge .
```

## Catturare video in un file

Il bridge scrive **solo H.264** su stdout e i log su stderr. Per salvare un file video **non usare `2>&1`**: unirebbe i log al bitstream e il file risulterebbe corrotto ("No start code is found", schermo grigio).

```bash
# Salva solo H.264 (nessun log nel file)
./legacybridge -host 192.168.1.10 -user ebio -pass 'xxx' -channel 1 -protocol-channel 0 2>/dev/null | head -c 3000000 > test.h264
ffplay test.h264
```

Con log in un file separato:

```bash
./legacybridge ... -verbose 2>log.txt | head -c 3000000 > test.h264
```

## Test offline

Usa un dump raw della `6002` gia' catturato:

```bash
./legacybridge --dump ../artifacts/video/channel5_dump.bin > ../artifacts/video/test.h264
ffprobe ../artifacts/video/test.h264
```

## Uso live

Esempio con canale utente `1`, che nel setup tipico viene mappato al canale protocollo `0`:

```bash
./legacybridge \
  --host 192.168.1.10 \
  --user Admin \
  --pass 'PASSWORD' \
  --channel 1 \
  > camera1.h264
```

## Mappatura canali

Dai dump `captures/NEW` risulta questa mappatura:

- canale utente `1` -> protocollo `0`
- canale utente `2` -> protocollo `1`
- canale utente `3` -> protocollo `2`
- canale utente `4` -> protocollo `3`
- canale utente `5` -> protocollo `4`

Per questo il default e':

```text
protocol-channel = channel - 1
```

Se il DVR usa una numerazione diversa, forza il canale protocollo:

```bash
./legacybridge --host 192.168.1.10 --user Admin --pass 'PASSWORD' --protocol-channel 1
```

## go2rtc

Il bridge scrive H.264 Annex-B su **stdout**, quindi va usato come sorgente `exec` di go2rtc.

### Configurazione con Docker (consigliata)

Nel repo c'è già tutto per usare go2rtc + legacybridge:

- **docker/docker-compose.yaml** – build dell’immagine con go2rtc + legacybridge
- **docker/config/go2rtc.yaml** – stream `dvr_cam1`…`dvr_cam5` che chiamano `run_legacybridge <canale>`
- **docker/run_legacybridge.sh** – wrapper che passa `DVR_IP`, `DVR_USER`, `DVR_PASSWORD` e il canale da env/argomento

Variabili d’ambiente (in `.env` o nel compose):

- `DVR_IP` – IP del DVR
- `DVR_USER` / `DVR_PASSWORD` – credenziali
- `DVR_CMD_PORT` (default 6001), `DVR_DATA_PORT` (default 6002)
- `DVR_STREAM` (default 0) – indice stream richiesto al DVR (`0` main, `1` sub-stream)
- `DVR_KEEPALIVE` (default `1s`) – keepalive verso il DVR
- `DVR_RECONNECT` (default `3s`) – retry reconnect bridge
- `DVR_VERBOSE` (default `0`) – se `1`, abilita log verbose del bridge

Avvio:

```bash
cd docker
cp .env.example .env   # poi modifica .env con IP e password
docker compose up -d --build
```

go2rtc espone gli stream su RTSP (porta 8554), HTTP/WebRTC (1984/8555). Puoi aprire l’interfaccia go2rtc (es. `http://<host>:1984`) e vedere `dvr_cam1`…`dvr_cam5`.

Profili aggiuntivi in `docker/config/go2rtc.yaml`:

- `dvr_camX_main` – stream principale (`stream=0`)
- `dvr_camX_sub` – substream a latenza piu' bassa (`stream=1`)
- `dvr_camX_auto` – prova substream e fallback al main

Diagnostica rapida:

```bash
cd docker
./diag.sh
```

### Configurazione manuale (senza Docker)

Se hai go2rtc installato a parte e il binario `legacybridge` in PATH (o path assoluto):

```yaml
# go2rtc.yaml
streams:
  dvr_cam1: exec:/path/to/legacybridge --host 192.168.1.10 --user Admin --pass 'PASSWORD' --channel 1
  dvr_cam2: exec:/path/to/legacybridge --host 192.168.1.10 --user Admin --pass 'PASSWORD' --channel 2
  # ...
```

Oppure con `run_legacybridge` e env: vedi `legacybridge/go2rtc.example.yaml` (formato con path `/config/legacybridge` per uso in container).

## Home Assistant

Una volta definiti gli stream in `go2rtc`, puoi:

- usare l'integrazione `go2rtc`
- oppure usare la WebRTC card puntando agli stream `dvr_cam1..dvr_cam5`

## Risoluzione problemi (schermo grigio / corrupted macroblock)

Se ffprobe riconosce il flusso (es. 704x480, 25 fps) ma ffplay mostra schermo grigio e messaggi tipo `corrupted macroblock`, `cbp too large`, `missing picture`:

1. **Prefisso 12 byte su tutti i pacchetti (default)** – Dall’analisi dei dump (dump2/dump3 pcapng), **ogni** pacchetto media (seq=1 e seq=2 con dati) ha il prefisso di 12 byte (`a7 72 4a 69...`); la continuazione NAL è anch’essa dopo 12 byte. Il bridge ora usa `payload[12:]` per le continuazioni di default. Se il video è ancora grigio, prova `--continuation-no-prefix` (solo se il tuo DVR invia continuazioni senza prefisso).

2. **Modalità come Python** – Stessa logica con sync al primo startcode:
   ```bash
   ./legacybridge ... --all-12
   ```
   Usa prefisso 12 byte su ogni pacchetto e sync al primo startcode (come `main.py`).

3. **Continuazione e mini-header** – Con `--continuation-no-prefix` puoi saltare N byte con `--continuation-skip=0|4|8`. Se vedi "corrupted macroblock", prova anche `--first-packet-trim=4`.

4. **Prova un altro offset** – alcuni DVR usano 8 byte di prefisso invece di 12:
   ```bash
   ./legacybridge ... --media-offset 8 2>/dev/null | head -c 3000000 > test.h264
   ffplay test.h264
   ```
   (Usa `2>/dev/null` per non mescolare i log con l'H.264 nel file.)
   Puoi provare anche `--media-offset 16`.

2. **Prova l’altro canale protocollo** – per la prima camera prova sia `--protocol-channel 0` sia `--protocol-channel 1`; la mappatura dipende dal firmware.

6. **Verifica i primi byte** – per vedere se il file inizia con Annex-B:
   ```bash
   hexdump -C test.h264 | head -25
   ```
   (Se hai `xxd`: `xxd test.h264 | head -20`). Cerca `00 00 00 01` o `00 00 01` seguito da `67` (SPS), `68` (PPS), `65` (IDR).

## Configurazione DVR (Life D/N/I 2013 — rilevata)

Impostazioni osservate sull'interfaccia del DVR. Utili per capire il flusso effettivo.

### Formato generale
- Formato video: **PAL**

### Impostazioni canali 1–5 (schermata "Channel Setup")
| Parametro      | Valore        |
|----------------|---------------|
| Risoluzione    | D1            |
| Framerate      | 25 fps        |
| Stream value   | 2560 kbps     |
| Stream type    | Video stream  |

### Impostazioni stream principale (schermata "Encode")
| Parametro          | Valore     |
|--------------------|------------|
| Risoluzione        | 1080p      |
| Framerate          | 25 fps     |
| Tipo bitrate       | CBR        |
| Bitrate            | 2048 kbps  |
| I-frame interval   | 1 s        |

### Sub-stream
| Parametro          | Valore     |
|--------------------|------------|
| Risoluzione        | CIF        |
| Framerate          | 5 fps      |
| Tipo bitrate       | CBR        |
| Bitrate            | 128 kbps   |
| I-frame interval   | 1 s        |
| Sub-stream abilitato | sì       |
| Framerate sub      | 30 fps     |
| Stream value sub   | 510 kbps   |

### Osservazioni sul flusso effettivo (rilevato da ffprobe)
- Risoluzione ricevuta: **704×480** (D1 NTSC) — non corrisponde né a 1080p né a CIF configurato
- Macroblock per frame: **1320** (44×30), confermato da errori ffplay (`concealing 1320 DC errors`)
- Il DVR usa probabilmente 704×480 come encoder interno indipendente dall'impostazione UI
- I-frame ogni **1 secondo** → ogni 25 frame a 25fps = 1 IDR ogni 25 P-frame
- Ogni frame IDR è composto da: 1 pacchetto `seq=0` (468 byte utili) + N pacchetti `seq=2` continuation (468 byte ciascuno)
- I P-frame arrivano come singolo pacchetto `seq=1` (468 byte utili)
- I keepalive del DVR arrivano come `seq=2` con `payloadLen=0` ogni ~80ms

## Note

- Il bridge esporta solo video H.264 (l’audio viene richiesto al DVR ma non è ancora pubblicato).
- Playback remoto: non ancora integrato in `legacybridge` in modo stabile. Per reverse/prova rapida usa `src/python/playback_probe.py` (modalita' `scan` e `play`) dal root del repository.
- Il protocollo richiede **ACK su ogni pacchetto media** (client → DVR, seq=2): senza questi ACK il flusso va fuori sync e il video risulta grigio/corrotto. Il bridge li invia automaticamente.
- Se go2rtc chiude stdout (stream disconnesso), il processo termina e verrà riavviato da go2rtc alla richiesta successiva.
