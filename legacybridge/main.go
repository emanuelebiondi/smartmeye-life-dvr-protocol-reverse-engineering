package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var magic = []byte{0x5a, 0x5a, 0xaa, 0x55}

// #region agent log
const debugLogPath = "/home/ebio/Downloads/csm_replication/.cursor/debug-b0461e.log"

func dbgLog(hypID, location, message string, data map[string]interface{}) {
	f, err := os.OpenFile(debugLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	ts := time.Now().UnixMilli()
	pairs := fmt.Sprintf(`"sessionId":"b0461e","hypothesisId":"%s","location":"%s","message":"%s","timestamp":%d`, hypID, location, message, ts)
	var dataParts string
	for k, v := range data {
		dataParts += fmt.Sprintf(`,"%s":"%v"`, k, v)
	}
	fmt.Fprintf(f, "{%s%s}\n", pairs, dataParts)
}

// #endregion

const (
	headerLen       = 32
	mediaHeaderLen  = 44
	mediaPrefixLen  = 12
	loginSession    = 0xffffffff
	keepAliveCmd    = 0x0000
	keepAliveSeq    = 801
	mediaAckSession = 0x64 // session usata negli ACK client->server nel dump (0x64=100)
	mediaAckFlag    = 0x02 // flag negli ACK (dump: client->server seq=2 flag=0x00000002)
)

type config struct {
	host             string
	cmdPort          int
	dataPort         int
	username         string
	password         string
	stream           int
	channel          int
	channelBase      int
	protocolChannel  int
	keepAlive        time.Duration
	reconnect        time.Duration
	dumpFile         string
	includeSeq2      bool
	mediaOffset      int  // byte dopo cui inizia H.264 nel payload (default 12; prova 8 o 16 se video grigio)
	continuation0    bool // se true, i pacchetti di continuazione senza prefisso 12 byte
	continuationSkip int  // byte da saltare all'inizio continuazione (0, 4, 8)
	firstPacketTrim  int  // byte da togliere alla fine del primo pacchetto (0 o 4)
	all12            bool // come Python: ogni pacchetto ha prefisso 12 byte, sync al primo startcode (qualsiasi NAL)
	strictFrameType  bool // state-machine stile DLL: frame_type validi {0,1,2,800}
	diagFile         string
	verbose          bool
}

type frame struct {
	cmd     uint32
	seq     uint32
	flag    uint32
	session uint32
	extra   uint32
	payload []byte
}

type client struct {
	cfg                 config
	logger              *log.Logger
	cmdConn             net.Conn
	dataConn            net.Conn
	dataReader          *bufio.Reader
	mu                  sync.Mutex
	diagMu              sync.Mutex
	diagWriter          *bufio.Writer
	diagFile            *os.File
	syncedPayloadOffset int  // offset usato al sync; per le continuazioni usiamo lo stesso (evita mix 8/12 byte).
	lastWasIDRStart     bool // true = ultimo pacchetto scritto era seq=0 (IDR start), continuazioni seq=2 vanno scritte
	haveOpenFrame       bool
}

func main() {
	cfg := parseFlags()
	loggerOut := io.Writer(io.Discard)
	if cfg.verbose {
		loggerOut = os.Stderr
	}
	logger := log.New(loggerOut, "legacybridge: ", log.LstdFlags)

	if cfg.dumpFile != "" {
		if err := extractDump(cfg.dumpFile, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cl := &client{cfg: cfg, logger: logger}
	if err := cl.run(ctx, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.host, "host", "192.168.1.10", "IP del DVR")
	flag.IntVar(&cfg.cmdPort, "cmd-port", 6001, "Porta comandi")
	flag.IntVar(&cfg.dataPort, "data-port", 6002, "Porta media")
	flag.StringVar(&cfg.username, "user", "Admin", "Username DVR")
	flag.StringVar(&cfg.password, "pass", "", "Password DVR")
	flag.IntVar(&cfg.channel, "channel", 1, "Canale lato utente")
	flag.IntVar(&cfg.channelBase, "channel-base", 0, "Base dei canali utente (0 => canali 1..N mappati a 1..N)")
	flag.IntVar(&cfg.protocolChannel, "protocol-channel", -1, "Indice canale lato protocollo; se -1 viene derivato da channel/channel-base")
	flag.IntVar(&cfg.stream, "stream", 0, "Indice stream")
	flag.DurationVar(&cfg.keepAlive, "keepalive", time.Second, "Periodo keepalive")
	flag.DurationVar(&cfg.reconnect, "reconnect", 3*time.Second, "Attesa prima di riconnettere")
	flag.StringVar(&cfg.dumpFile, "dump", "", "Legge un dump raw della 6002 e scrive H264 su stdout")
	flag.BoolVar(&cfg.includeSeq2, "include-seq2", false, "Include anche frame seq=2/3 dal flusso media")
	flag.IntVar(&cfg.mediaOffset, "media-offset", 12, "Offset byte payload media prima dell'H.264 (12 default; prova 8 o 16 se video grigio)")
	// Dai dump: seq=1 e seq=2 con payload hanno sempre prefisso 12 byte (a7724a69...); continuazione = payload[12:].
	flag.BoolVar(&cfg.continuation0, "continuation-no-prefix", false, "Se true, continuazione senza prefisso (payload[0:]); default false = sempre 12 byte come dump")
	flag.IntVar(&cfg.continuationSkip, "continuation-skip", 0, "Byte da saltare all'inizio continuazione (default 0, prefisso già gestito)")
	flag.IntVar(&cfg.firstPacketTrim, "first-packet-trim", 0, "Byte da togliere alla fine del primo pacchetto (prova 4 se MB corrotti)")
	flag.BoolVar(&cfg.all12, "all-12", true, "Prefisso 12 byte su ogni pacchetto, sync al primo startcode (default; come main.py)")
	flag.BoolVar(&cfg.strictFrameType, "strict-frame-type", true, "Applica parsing media stile DLL (frame_type 0/1/2/800)")
	flag.StringVar(&cfg.diagFile, "diag-file", "", "Scrive diagnostica media CSV-like (cmd,frame_type,seqid,len,meta,action)")
	flag.BoolVar(&cfg.verbose, "verbose", false, "Abilita log verbosi su stderr")
	flag.Parse()
	return cfg
}

func (c *client) run(ctx context.Context, out io.Writer) error {
	for {
		err := c.runOnce(ctx, out)
		if err == nil || errors.Is(err, context.Canceled) {
			return err
		}
		if isBrokenPipe(err) {
			return nil
		}
		if c.cfg.verbose {
			c.logger.Printf("stream terminato: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.cfg.reconnect):
		}
	}
}

func (c *client) runOnce(ctx context.Context, out io.Writer) error {
	protoCh := c.protocolChannel()
	if protoCh < 0 || protoCh > 7 {
		return fmt.Errorf("protocol-channel fuori range: %d", protoCh)
	}
	if err := c.openDiag(); err != nil {
		return err
	}
	defer c.closeDiag()

	if err := c.connect(); err != nil {
		return err
	}
	defer c.close()

	loginReply, err := c.login()
	if err != nil {
		return err
	}
	if c.cfg.verbose {
		c.logger.Printf("login ok: %s", compactXML(loginReply.payload))
	}

	drainDone := make(chan error, 1)
	go func() {
		drainDone <- c.drainCmd(ctx)
	}()

	if err := c.bootstrap(); err != nil {
		return err
	}

	// Come Python e dump: stream_ch_request PRIMA di leggere socket_id dalla media, altrimenti il DVR non invia il flusso.
	if err := c.sendStreamChRequest(protoCh); err != nil {
		return err
	}

	socketID, err := c.readSocketID()
	if err != nil {
		return err
	}
	if c.cfg.verbose {
		c.logger.Printf("channel=%d protocol=%d socket_id=%d", c.cfg.channel, protoCh, socketID)
	}

	if err := c.openChannel(protoCh, socketID); err != nil {
		return err
	}

	keepDone := make(chan error, 1)
	go func() {
		keepDone <- c.keepAliveLoop(ctx)
	}()

	streamErr := c.streamMedia(ctx, out)

	select {
	case err := <-keepDone:
		if err != nil && !errors.Is(err, context.Canceled) && !isBrokenPipe(err) {
			if c.cfg.verbose {
				c.logger.Printf("keepalive: %v", err)
			}
		}
	default:
	}

	select {
	case err := <-drainDone:
		if err != nil && !errors.Is(err, context.Canceled) && c.cfg.verbose {
			c.logger.Printf("cmd drain: %v", err)
		}
	default:
	}

	return streamErr
}

func (c *client) openDiag() error {
	if c.cfg.diagFile == "" {
		return nil
	}
	f, err := os.OpenFile(c.cfg.diagFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open diag-file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat diag-file: %w", err)
	}
	c.diagFile = f
	c.diagWriter = bufio.NewWriterSize(f, 64*1024)
	if info.Size() == 0 {
		fmt.Fprintln(c.diagWriter, "ts_ms cmd frame_type flag session seqid payload_len meta0 meta1 meta2 action prefix16")
		_ = c.diagWriter.Flush()
	}
	return nil
}

func (c *client) closeDiag() {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	if c.diagWriter != nil {
		_ = c.diagWriter.Flush()
		c.diagWriter = nil
	}
	if c.diagFile != nil {
		_ = c.diagFile.Close()
		c.diagFile = nil
	}
}

func (c *client) writeDiagMedia(cmd, frameType, flag, session, seqid uint32, payload []byte, action string) {
	if c.diagWriter == nil {
		return
	}
	meta0, meta1, meta2 := uint32(0), uint32(0), uint32(0)
	if len(payload) >= mediaPrefixLen {
		meta0 = be32le(payload[0:4])
		meta1 = be32le(payload[4:8])
		meta2 = be32le(payload[8:12])
	}
	prefix := payload
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	fmt.Fprintf(
		c.diagWriter,
		"%d %d %d %d %d %d %d %d %d %d %s %x\n",
		time.Now().UnixMilli(),
		cmd, frameType, flag, session, seqid, len(payload),
		meta0, meta1, meta2, action, prefix,
	)
}

func (c *client) protocolChannel() int {
	if c.cfg.protocolChannel >= 0 {
		return c.cfg.protocolChannel
	}
	return c.cfg.channel - c.cfg.channelBase
}

func (c *client) connect() error {
	dialer := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	var err error
	c.cmdConn, err = dialer.Dial("tcp", net.JoinHostPort(c.cfg.host, strconv.Itoa(c.cfg.cmdPort)))
	if err != nil {
		return fmt.Errorf("connect cmd: %w", err)
	}
	c.dataConn, err = dialer.Dial("tcp", net.JoinHostPort(c.cfg.host, strconv.Itoa(c.cfg.dataPort)))
	if err != nil {
		c.cmdConn.Close()
		c.cmdConn = nil
		return fmt.Errorf("connect data: %w", err)
	}
	c.dataReader = bufio.NewReaderSize(c.dataConn, 64*1024)
	return nil
}

func (c *client) close() {
	if c.cmdConn != nil {
		c.cmdConn.Close()
		c.cmdConn = nil
	}
	if c.dataConn != nil {
		c.dataConn.Close()
		c.dataConn = nil
	}
	c.dataReader = nil
}

func (c *client) login() (*frame, error) {
	xml := fmt.Sprintf(`<?xml version="1.0" ?>
<Message Version="1">
    <Header>
        <login_request username="%s" password="%s" />
    </Header>
</Message>
`, c.cfg.username, c.cfg.password)
	if err := c.writeCmd(buildFrame(0x56F5, 100, 0, loginSession, 0, []byte(xml+"\x00"))); err != nil {
		return nil, err
	}

	for {
		f, err := readFrame(c.cmdConn)
		if err != nil {
			return nil, fmt.Errorf("login recv: %w", err)
		}
		if f.cmd == 0x56F5 {
			return f, nil
		}
		if c.cfg.verbose {
			c.logger.Printf("login skip cmd=0x%04x seq=%d", f.cmd, f.seq)
		}
	}
}

func (c *client) bootstrap() error {
	seqs := []struct {
		cmd   uint32
		seq   uint32
		extra uint32
	}{
		{0x56F6, 1100, 0x00},
		{0x56F7, 1108, 0x01},
		{0x56F8, 1201, 0x00},
		{0x56F9, 1100, 0x00},
		{0x56FA, 1024, 0xFF},
		{0x56FB, 1010, 0x01},
		{0x56FC, 1010, 0x02},
		{0x56FD, 1010, 0x04},
		{0x56FE, 1010, 0x08},
		{0x56FF, 1010, 0x10},
		{0x5700, 1010, 0x20},
		{0x5701, 1010, 0x40},
		{0x5702, 1010, 0x80},
	}
	for _, item := range seqs {
		if err := c.writeCmd(buildFrame(item.cmd, item.seq, 0, 0, item.extra, nil)); err != nil {
			return err
		}
		time.Sleep(30 * time.Millisecond)
	}
	for i := 0; i < 6; i++ {
		if err := c.writeCmd(buildFrame(keepAliveCmd, keepAliveSeq, 1, 0, 0, nil)); err != nil {
			return err
		}
		time.Sleep(30 * time.Millisecond)
	}
	return nil
}

// sendStreamChRequest invia stream_ch_request (come Python/dump) prima di leggere socket_id dalla media.
func (c *client) sendStreamChRequest(protoCh int) error {
	base := uint32(0x5703 + protoCh*3)
	// #region agent log
	dbgLog("H4", "sendStreamChRequest", "sending hdr-only", map[string]interface{}{"cmd": fmt.Sprintf("0x%04x", base), "protoCh": protoCh})
	// #endregion
	if err := c.writeCmd(buildFrame(base, 1108, 0, 0, 0, nil)); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	streamChXML := fmt.Sprintf(`<?xml version="1.0" ?>
<Message Version="1">
    <Header>
        <stream_ch_request channel="%d" stream="%d" />
    </Header>
</Message>
`, protoCh, c.cfg.stream)
	// #region agent log
	dbgLog("H4", "sendStreamChRequest", "sending stream_ch_request XML", map[string]interface{}{"cmd": fmt.Sprintf("0x%04x", base), "xml_snippet": fmt.Sprintf("channel=%d stream=%d", protoCh, c.cfg.stream)})
	// #endregion
	if err := c.writeCmd(buildFrame(base, 1201, 0, 0, 0, []byte(streamChXML+"\x00"))); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	return nil
}

func (c *client) readSocketID() (uint32, error) {
	for {
		f, err := readFrame(c.dataReader)
		if err != nil {
			// #region agent log
			dbgLog("H2", "readSocketID", "readFrame error", map[string]interface{}{"err": err.Error()})
			// #endregion
			return 0, fmt.Errorf("data first frame: %w", err)
		}
		// #region agent log
		dbgLog("H2", "readSocketID", "got frame", map[string]interface{}{"cmd": fmt.Sprintf("0x%04x", f.cmd), "seq": f.seq, "payloadLen": len(f.payload)})
		// #endregion
		if len(f.payload) == 4 {
			socketID := be32le(f.payload)
			// #region agent log
			dbgLog("H2", "readSocketID", "socket_id received", map[string]interface{}{"socketID": socketID})
			// #endregion
			// Ack iniziale come da dump (client->server seq=800, payload socket_id)
			ack := buildFrame(0, 800, 0, 0, 0, f.payload)
			if err := c.writeData(ack); err != nil {
				return 0, fmt.Errorf("media ack socket_id: %w", err)
			}
			return socketID, nil
		}
	}
}

func (c *client) openChannel(protoCh int, socketID uint32) error {
	extra := uint32(1 << protoCh)
	base := uint32(0x5703 + protoCh*3)
	// stream_ch già inviato in sendStreamChRequest; qui solo open_channel e open_audio

	openXML := fmt.Sprintf(`<?xml version="1.0" ?>
<Message Version="1">
    <Header>
        <open_channel_request socket="%d" channel="%d" stream="%d" />
    </Header>
</Message>
`, socketID, protoCh, c.cfg.stream)
	// #region agent log
	dbgLog("H1", "openChannel", "open_channel_request XML", map[string]interface{}{"cmd": fmt.Sprintf("0x%04x", base+1), "socketID": socketID, "xml": fmt.Sprintf("socket=%d channel=%d stream=%d", socketID, protoCh, c.cfg.stream)})
	// #endregion
	if err := c.writeCmd(buildFrame(base+1, 200, 0, 0, extra, []byte(openXML+"\x00"))); err != nil {
		return err
	}

	audioXML := fmt.Sprintf(`<?xml version="1.0" ?>
<Message Version="1">
    <Header>
        <open_audio_request socket="%d" channel="%d" stream="%d" />
    </Header>
</Message>
`, socketID, protoCh, c.cfg.stream)
	if err := c.writeCmd(buildFrame(base+2, 1303, 0, 0, extra, []byte(audioXML+"\x00"))); err != nil {
		return err
	}

	return nil
}

func (c *client) drainCmd(ctx context.Context) error {
	for {
		if err := c.cmdConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			return err
		}
		f, err := readFrame(c.cmdConn)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				continue
			}
			return err
		}
		if c.cfg.verbose {
			if len(f.payload) > 0 && bytes.Contains(f.payload, []byte("<Message")) {
				c.logger.Printf("cmd<- 0x%04x seq=%d extra=0x%x %s", f.cmd, f.seq, f.extra, compactXML(f.payload))
			} else {
				c.logger.Printf("cmd<- 0x%04x seq=%d extra=0x%x len=%d", f.cmd, f.seq, f.extra, len(f.payload))
			}
		}
	}
}

func (c *client) keepAliveLoop(ctx context.Context) error {
	t := time.NewTicker(c.cfg.keepAlive)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := c.writeCmd(buildFrame(keepAliveCmd, keepAliveSeq, 1, 0, 0, nil)); err != nil {
				return err
			}
		}
	}
}

type streamState int

const (
	stateSearchingConfig streamState = iota
	stateStreaming
)

func (c *client) streamMedia(ctx context.Context, out io.Writer) error {
	state := stateSearchingConfig
	var frameBuffer [][]byte
	c.haveOpenFrame = false
	c.lastWasIDRStart = false
	pending := make([]byte, 0, 256*1024)
	tmp := make([]byte, 64*1024)
	protoCh := uint32(c.protocolChannel())

	for {
		if err := c.dataConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		n, err := c.dataReader.Read(tmp)
		if n > 0 {
			pending = append(pending, tmp[:n]...)
			var consumed int
			consumed, state, err = c.processMediaFrames(pending, out, &frameBuffer, state, protoCh)
			if err != nil {
				return err
			}
			if consumed > 0 {
				pending = append(pending[:0], pending[consumed:]...)
			}
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				continue
			}
			return err
		}
	}
}

// #region agent log
var mediaPacketCount int

// #endregion

func (c *client) processMediaFrames(pending []byte, out io.Writer, frameBuffer *[][]byte, state streamState, protoCh uint32) (int, streamState, error) {
	_ = frameBuffer
	i := 0
	for i+mediaHeaderLen <= len(pending) {
		if !bytes.Equal(pending[i:i+4], magic) {
			j := bytes.Index(pending[i+1:], magic)
			if j < 0 {
				if len(pending) > 3 {
					return len(pending) - 3, state, nil
				}
				return 0, state, nil
			}
			i += j + 1
			continue
		}
		// Porta 6002 (media) usa header esteso da 44 byte:
		// - primi 32 byte come frame standard
		// - +12 byte meta
		// - lunghezza payload a offset 40
		payloadLen := int(be32le(pending[i+40 : i+44]))
		if payloadLen < 0 || payloadLen > 8*1024*1024 {
			i++
			continue
		}
		frameLen := mediaHeaderLen + payloadLen
		if len(pending) < i+frameLen {
			break
		}
		frameBytes := pending[i : i+frameLen]
		cmd := be32le(frameBytes[4:8])
		frameType := be32le(frameBytes[8:12]) // nei DLL e' frame_type (0/1/2), non sequence globale
		flag := be32le(frameBytes[12:16])
		session := be32le(frameBytes[16:20])
		seqID := be32le(frameBytes[20:24])
		payload := frameBytes[mediaHeaderLen:]

		// #region agent log
		mediaPacketCount++
		if mediaPacketCount <= 10 {
			prefix := ""
			if len(payload) >= 16 {
				prefix = fmt.Sprintf("%02x%02x%02x%02x %02x%02x%02x%02x %02x%02x%02x%02x %02x%02x%02x%02x",
					payload[0], payload[1], payload[2], payload[3],
					payload[4], payload[5], payload[6], payload[7],
					payload[8], payload[9], payload[10], payload[11],
					payload[12], payload[13], payload[14], payload[15])
			} else if len(payload) > 0 {
				prefix = fmt.Sprintf("%x", payload)
			}
			dbgLog("H2H3", "processMediaFrames", "media packet", map[string]interface{}{
				"n": mediaPacketCount, "cmd": cmd, "frameType": frameType, "protoCh": protoCh,
				"payloadLen": payloadLen, "prefix": prefix,
			})
		}
		// #endregion

		// Ack ogni pacchetto media (seq=2, flag=2) come nel dump client->server.
		_ = c.writeData(buildFrame(cmd, 2, mediaAckFlag, mediaAckSession, 0, nil))
		c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "recv")

		if c.cfg.strictFrameType {
			if frameType != 0 && frameType != 1 && frameType != 2 && frameType != 800 {
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_invalid_frame_type")
				i += frameLen
				continue
			}
		}

		if cmd != protoCh {
			c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_other_channel")
			i += frameLen
			continue
		}

		// Pacchetti vuoti = keepalive/heartbeat DVR.
		if payloadLen == 0 {
			c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "keepalive_or_empty")
			i += frameLen
			continue
		}
		switch state {
		case stateSearchingConfig:
			// Nel decoder DLL la sync iniziale avviene su frame_type=0 (I-frame/config start)
			if frameType != 0 {
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "wait_sync_type0")
				i += frameLen
				continue
			}
			start := startcodeOffset(payload)
			if start < 0 || start >= len(payload) {
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "wait_sync_no_startcode")
				i += frameLen
				continue
			}
			toOut := payload[start:]
			if !isKeyframeOrConfig(toOut) {
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "wait_sync_not_keyframe")
				i += frameLen
				continue
			}
			if trim := c.cfg.firstPacketTrim; trim > 0 && len(toOut) > trim {
				toOut = toOut[:len(toOut)-trim]
			}
			if len(toOut) > 0 {
				if _, err := out.Write(toOut); err != nil {
					return i, state, err
				}
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "write_sync")
			}
			c.haveOpenFrame = true
			c.lastWasIDRStart = true
			state = stateStreaming
		case stateStreaming:
			switch frameType {
			case 0, 1:
				// Nuovo frame: il payload inizia con prefisso 12 byte e poi Annex-B.
				start := startcodeOffset(payload)
				if start < 0 || start >= len(payload) {
					c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_start_no_startcode")
					i += frameLen
					continue
				}
				toOut := payload[start:]
				if len(toOut) == 0 {
					c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_empty_start")
					i += frameLen
					continue
				}
				if _, err := out.Write(toOut); err != nil {
					return i, state, err
				}
				c.haveOpenFrame = true
				c.lastWasIDRStart = frameType == 0
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "write_start")
			case 2:
				// Nei dump NEW type=2 e' un blocco meta (tipicamente 164 byte), non H264 Annex-B.
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "ignore_type2")
			case 800:
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "ignore_socket_id")
			default:
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_unknown")
			}
		}
		i += frameLen
	}
	return i, state, nil
}

func (c *client) writeCmd(buf []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmdConn == nil {
		return net.ErrClosed
	}
	_ = c.cmdConn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err := c.cmdConn.Write(buf)
	if c.cfg.verbose && err == nil {
		cmd := be32le(buf[4:8])
		seq := be32le(buf[8:12])
		extra := be32le(buf[20:24])
		c.logger.Printf("cmd-> 0x%04x seq=%d extra=0x%x len=%d", cmd, seq, extra, len(buf)-headerLen)
	}
	return err
}

// writeData invia un frame sulla connessione media (es. ACK seq=2). Necessario per tenere il DVR in sync.
func (c *client) writeData(buf []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dataConn == nil {
		return net.ErrClosed
	}
	_ = c.dataConn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err := c.dataConn.Write(buf)
	return err
}

func buildFrame(cmd, seq, flag, session, extra uint32, payload []byte) []byte {
	out := make([]byte, 0, headerLen+len(payload))
	out = append(out, magic...)
	out = appendLE32(out, cmd)
	out = appendLE32(out, seq)
	out = appendLE32(out, flag)
	out = appendLE32(out, session)
	out = appendLE32(out, extra)
	out = appendLE32(out, 0)
	out = appendLE32(out, uint32(len(payload)))
	out = append(out, payload...)
	return out
}

func readFrame(r io.Reader) (*frame, error) {
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	if !bytes.Equal(hdr[:4], magic) {
		return nil, fmt.Errorf("bad magic: %x", hdr[:4])
	}
	payloadLen := be32le(hdr[28:32])
	payload := make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}
	return &frame{
		cmd:     be32le(hdr[4:8]),
		seq:     be32le(hdr[8:12]),
		flag:    be32le(hdr[12:16]),
		session: be32le(hdr[16:20]),
		extra:   be32le(hdr[20:24]),
		payload: payload,
	}, nil
}

func readFrameResync(r *bufio.Reader) (*frame, error) {
	for {
		hdr, err := readHeaderResync(r)
		if err != nil {
			return nil, err
		}
		payloadLen := be32le(hdr[28:32])
		payload := make([]byte, payloadLen)
		if payloadLen > 0 {
			if _, err := io.ReadFull(r, payload); err != nil {
				return nil, err
			}
		}
		return &frame{
			cmd:     be32le(hdr[4:8]),
			seq:     be32le(hdr[8:12]),
			flag:    be32le(hdr[12:16]),
			session: be32le(hdr[16:20]),
			extra:   be32le(hdr[20:24]),
			payload: payload,
		}, nil
	}
}

func readHeaderResync(r *bufio.Reader) ([]byte, error) {
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b != magic[0] {
			continue
		}
		peek, err := r.Peek(3)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(peek, magic[1:]) {
			continue
		}
		hdr := make([]byte, headerLen)
		hdr[0] = b
		copy(hdr[1:4], peek)
		if _, err := io.ReadFull(r, hdr[4:]); err != nil {
			return nil, err
		}
		return hdr, nil
	}
}

func extractDump(path string, out io.Writer) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var modern bytes.Buffer
	if written, err := extractDumpMedia44(src, &modern); err == nil && written > 0 {
		_, werr := out.Write(modern.Bytes())
		return werr
	}
	var legacy bytes.Buffer
	if err := extractDumpLegacy32(src, &legacy); err != nil {
		return err
	}
	_, err = out.Write(legacy.Bytes())
	return err
}

// extractDumpMedia44 estrae H264 da dump media in formato legacy NEW:
// header 44 byte (32 standard + 12 meta) e payload_len a offset 40.
func extractDumpMedia44(src []byte, out io.Writer) (int, error) {
	i := 0
	written := 0
	synced := false
	streamCmd := uint32(0xffffffff)

	// Primo frame media puo' essere socket_id con header standard 32 byte.
	if len(src) >= headerLen && bytes.Equal(src[:4], magic) {
		firstLen := int(be32le(src[28:32]))
		firstType := be32le(src[8:12])
		if firstType == 800 && firstLen == 4 && len(src) >= headerLen+firstLen {
			i = headerLen + firstLen
		}
	}

	for i+mediaHeaderLen <= len(src) {
		if !bytes.Equal(src[i:i+4], magic) {
			j := bytes.Index(src[i+1:], magic)
			if j < 0 {
				break
			}
			i += j + 1
			continue
		}
		payloadLen := int(be32le(src[i+40 : i+44]))
		if payloadLen < 0 || payloadLen > 8*1024*1024 {
			i++
			continue
		}
		end := i + mediaHeaderLen + payloadLen
		if end > len(src) {
			break
		}
		cmd := be32le(src[i+4 : i+8])
		frameType := be32le(src[i+8 : i+12])
		payload := src[i+mediaHeaderLen : end]

		if frameType == 0 || frameType == 1 {
			start := startcodeOffset(payload)
			if start >= 0 && start < len(payload) {
				chunk := payload[start:]
				if !synced {
					if frameType != 0 || !isKeyframeOrConfig(chunk) {
						i = end
						continue
					}
					synced = true
					streamCmd = cmd
				}
				if cmd == streamCmd {
					n, werr := out.Write(chunk)
					written += n
					if werr != nil {
						return written, werr
					}
				}
			}
		}
		i = end
	}

	return written, nil
}

func extractDumpLegacy32(src []byte, out io.Writer) error {
	synced := false
	probeCount := 0
	for i := 0; i+headerLen <= len(src); {
		if !bytes.Equal(src[i:i+4], magic) {
			j := bytes.Index(src[i+1:], magic)
			if j < 0 {
				break
			}
			i += j + 1
			continue
		}
		payloadLen := int(be32le(src[i+28 : i+32]))
		end := i + headerLen + payloadLen
		if end > len(src) {
			break
		}
		seq := be32le(src[i+8 : i+12])
		payload := src[i+headerLen : end]
		if len(payload) == 0 {
			i = end
			continue
		}
		var buf []byte
		if seq <= 1 {
			// Primo frame di un NAL: strip prefisso 12 byte e cerca startcode Annex-B
			if len(payload) > mediaPrefixLen {
				buf = payload[mediaPrefixLen:]
			}
			if buf != nil && !synced {
				probeCount++
				start := annexBStart(buf)
				if start >= 0 {
					buf = buf[start:]
					synced = true
				} else if probeCount < 8 {
					i = end
					continue
				} else {
					synced = true
				}
			}
		} else {
			// Continuazione (seq=2 con dati): dai dump ha sempre prefisso 12 byte come seq=1
			if len(payload) > mediaPrefixLen {
				buf = payload[mediaPrefixLen:]
			}
		}
		if synced && len(buf) > 0 {
			if _, err := out.Write(buf); err != nil {
				return err
			}
		}
		i = end
	}
	return nil
}

func compactXML(b []byte) string {
	s := strings.TrimSpace(strings.ReplaceAll(string(bytes.TrimRight(b, "\x00")), "\n", " "))
	s = strings.Join(strings.Fields(s), " ")
	return s
}

func appendLE32(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func be32le(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || strings.Contains(strings.ToLower(err.Error()), "broken pipe")
}

func annexBStart(buf []byte) int {
	if i := bytes.Index(buf, []byte{0x00, 0x00, 0x00, 0x01}); i >= 0 {
		return i
	}
	if i := bytes.Index(buf, []byte{0x00, 0x00, 0x01}); i >= 0 {
		return i
	}
	return -1
}

// hasAnnexBStart indica se buf inizia con startcode Annex-B (4 o 3 byte) e ne restituisce la lunghezza.
func hasAnnexBStart(buf []byte) (bool, int) {
	if len(buf) >= 4 && buf[0] == 0 && buf[1] == 0 && buf[2] == 0 && buf[3] == 1 {
		return true, 4
	}
	if len(buf) >= 3 && buf[0] == 0 && buf[1] == 0 && buf[2] == 1 {
		return true, 3
	}
	return false, 0
}

// startcodeOffset cerca il primo startcode Annex-B seguito da un byte NAL valido (tipo 0-23).
// Restituisce l'indice dello startcode nel payload o -1.
func startcodeOffset(payload []byte) int {
	for i := 0; i < len(payload); i++ {
		if i+5 <= len(payload) && payload[i] == 0 && payload[i+1] == 0 && payload[i+2] == 0 && payload[i+3] == 1 {
			nalType := payload[i+4] & 0x1F
			if nalType <= 23 {
				return i
			}
		}
		if i+4 <= len(payload) && payload[i] == 0 && payload[i+1] == 0 && payload[i+2] == 1 {
			nalType := payload[i+3] & 0x1F
			if nalType <= 23 {
				return i
			}
		}
	}
	return -1
}

var annexBStartCode = []byte{0x00, 0x00, 0x00, 0x01}

// firstNALType restituisce il tipo (0-23) del primo NAL in dati Annex-B, o -1.
func firstNALType(annexB []byte) int {
	if len(annexB) < 4 {
		return -1
	}
	off := 0
	if annexB[0] == 0 && annexB[1] == 0 && annexB[2] == 0 && annexB[3] == 1 {
		off = 4
	} else if annexB[0] == 0 && annexB[1] == 0 && annexB[2] == 1 {
		off = 3
	} else {
		return -1
	}
	if off >= len(annexB) {
		return -1
	}
	return int(annexB[off] & 0x1F)
}

// isKeyframeOrConfig: true se il primo NAL è SPS(7), PPS(8) o IDR(5). Serve per non iniziare con P-frame.
func isKeyframeOrConfig(annexB []byte) bool {
	t := firstNALType(annexB)
	return t == 5 || t == 7 || t == 8
}

// avccToAnnexB converte payload in formato AVCC ([4-byte length][NAL]...) in Annex-B.
// be = true: length big-endian (MP4); be = false: little-endian.
func avccToAnnexBWithEndian(payload []byte, offset int, be bool) []byte {
	if offset+4 > len(payload) {
		return nil
	}
	out := make([]byte, 0, len(payload))
	p := payload[offset:]
	for len(p) >= 4 {
		var nalLen int
		if be {
			nalLen = int(p[0])<<24 | int(p[1])<<16 | int(p[2])<<8 | int(p[3])
		} else {
			nalLen = int(p[0]) | int(p[1])<<8 | int(p[2])<<16 | int(p[3])<<24
		}
		if nalLen <= 0 || nalLen > len(p)-4 {
			break
		}
		nal := p[4 : 4+nalLen]
		if len(nal) > 0 {
			nalType := nal[0] & 0x1F
			if nalType <= 23 {
				out = append(out, annexBStartCode...)
				out = append(out, nal...)
			}
		}
		p = p[4+nalLen:]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func avccToAnnexB(payload []byte, offset int) []byte {
	if b := avccToAnnexBWithEndian(payload, offset, true); len(b) > 0 {
		return b
	}
	return avccToAnnexBWithEndian(payload, offset, false)
}
