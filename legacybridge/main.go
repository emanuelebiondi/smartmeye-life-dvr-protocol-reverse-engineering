package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
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
	mediaAckSession = 0x64 // session used for client->server ACKs in captures (0x64=100)
	mediaAckFlag    = 0x02 // ACK flag (captures: client->server seq=2 flag=0x00000002)
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
	mediaOffset      int  // byte offset where H.264 starts in payload (default 12; try 8 or 16 if video is gray)
	continuation0    bool // if true, continuation packets are treated without the 12-byte prefix
	continuationSkip int  // bytes to skip at continuation start (0, 4, 8)
	firstPacketTrim  int  // bytes trimmed from end of first packet (0 or 4)
	all12            bool // Python-style: every packet has 12-byte prefix, sync on first start code (any NAL)
	strictFrameType  bool // DLL-style state machine: valid frame_type values {0,1,2,800}
	diagFile         string
	verbose          bool
	hubMode          bool
	hubChannels      string
	hubBind          string
	hubPortBase      int
	hubProtoOffset   int
	subscribeAddr    string
	channelMapSpec   string
	channelMap       map[int]int
	metricsAddr      string
	logJSON          bool
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
	metrics             *bridgeMetrics
	cmdConn             net.Conn
	dataConn            net.Conn
	dataReader          *bufio.Reader
	mu                  sync.Mutex
	diagMu              sync.Mutex
	diagWriter          *bufio.Writer
	diagFile            *os.File
	syncedPayloadOffset int  // offset chosen at sync; reuse for continuations (avoids 8/12-byte mixing).
	lastWasIDRStart     bool // true = last written packet was seq=0 (IDR start), so seq=2 continuations are written
	haveOpenFrame       bool
}

type hubChannel struct {
	user  int
	proto uint32
	port  int
}

type hubPublisher struct {
	ch    hubChannel
	ln    net.Listener
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	boot  []byte
	m     *bridgeMetrics
}

type bridgeMetrics struct {
	started       time.Time
	mu            sync.Mutex
	mode          string
	reconnects    uint64
	dropsByReason map[string]uint64
	framesByProto map[uint32]uint64
	bytesByProto  map[uint32]uint64
	syncByProto   map[uint32]uint64
	subsByProto   map[uint32]int
}

type jsonLogWriter struct {
	out io.Writer
}

func (w jsonLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	line, _ := json.Marshal(map[string]interface{}{
		"ts":   time.Now().Format(time.RFC3339Nano),
		"lvl":  "info",
		"msg":  msg,
		"src":  "legacybridge",
		"type": "runtime",
	})
	line = append(line, '\n')
	_, err := w.out.Write(line)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func main() {
	cfg := parseFlags()
	loggerOut := io.Writer(io.Discard)
	if cfg.verbose {
		loggerOut = os.Stderr
	}
	if cfg.logJSON && cfg.verbose {
		loggerOut = jsonLogWriter{out: os.Stderr}
	}
	logger := log.New(loggerOut, "legacybridge: ", log.LstdFlags)
	if cfg.logJSON {
		logger = log.New(loggerOut, "", 0)
	}

	chMap, err := parseChannelMap(cfg.channelMapSpec)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg.channelMap = chMap

	if cfg.dumpFile != "" {
		if err := extractDump(cfg.dumpFile, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mode := "single"
	if cfg.hubMode {
		mode = "hub"
	}
	if cfg.subscribeAddr != "" {
		mode = "subscribe"
	}
	m := newBridgeMetrics(mode)
	_ = startMetricsServer(ctx, cfg.metricsAddr, m, logger, cfg.verbose)

	cl := &client{cfg: cfg, logger: logger, metrics: m}
	if cfg.subscribeAddr != "" {
		if err := runSubscribe(ctx, cfg.subscribeAddr, os.Stdout, cfg.reconnect, logger, cfg.verbose, m); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if cfg.hubMode {
		if err := cl.runHub(ctx); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := cl.run(ctx, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.host, "host", "192.168.1.10", "DVR IP")
	flag.IntVar(&cfg.cmdPort, "cmd-port", 6001, "Command port")
	flag.IntVar(&cfg.dataPort, "data-port", 6002, "Media port")
	flag.StringVar(&cfg.username, "user", "Admin", "Username DVR")
	flag.StringVar(&cfg.password, "pass", "", "Password DVR")
	flag.IntVar(&cfg.channel, "channel", 1, "User-side channel")
	flag.IntVar(&cfg.channelBase, "channel-base", 0, "User channel base (0 => channels 1..N map to 1..N)")
	flag.IntVar(&cfg.protocolChannel, "protocol-channel", -1, "Protocol-side channel index; if -1 it is derived from channel/channel-base")
	flag.IntVar(&cfg.stream, "stream", 0, "Stream index")
	flag.DurationVar(&cfg.keepAlive, "keepalive", time.Second, "Keepalive period")
	flag.DurationVar(&cfg.reconnect, "reconnect", 3*time.Second, "Delay before reconnect")
	flag.StringVar(&cfg.dumpFile, "dump", "", "Read a raw 6002 dump and write H264 to stdout")
	flag.BoolVar(&cfg.includeSeq2, "include-seq2", true, "Include frame_type=2 continuation packets (recommended; disable only for incompatible firmware)")
	flag.IntVar(&cfg.mediaOffset, "media-offset", 12, "Media payload byte offset before H.264 (default 12; try 8 or 16 if video is gray)")
	// In captures, seq=1 and seq=2 packets with payload always include a 12-byte prefix (a7724a69...); continuation = payload[12:].
	flag.BoolVar(&cfg.continuation0, "continuation-no-prefix", false, "If true, continuation without prefix (payload[0:]); default false = always 12-byte prefix from captures")
	flag.IntVar(&cfg.continuationSkip, "continuation-skip", 0, "Bytes to skip at continuation start (default 0, prefix already handled)")
	flag.IntVar(&cfg.firstPacketTrim, "first-packet-trim", 0, "Bytes to trim from first packet end (try 4 if macroblocks are corrupted)")
	flag.BoolVar(&cfg.all12, "all-12", true, "12-byte prefix on every packet, sync on first start code (default; like main.py)")
	flag.BoolVar(&cfg.strictFrameType, "strict-frame-type", true, "Apply DLL-style media parsing (frame_type 0/1/2/800)")
	flag.StringVar(&cfg.diagFile, "diag-file", "", "Write CSV-like media diagnostics (cmd,frame_type,seqid,len,meta,action)")
	flag.BoolVar(&cfg.verbose, "verbose", false, "Enable verbose logs on stderr")
	flag.BoolVar(&cfg.hubMode, "hub", false, "Run as single-session DVR hub with per-channel local TCP outputs")
	flag.StringVar(&cfg.hubChannels, "hub-channels", "1,2,3,4,5", "Hub user channels (comma-separated)")
	flag.StringVar(&cfg.hubBind, "hub-bind", "127.0.0.1", "Hub bind address")
	flag.IntVar(&cfg.hubPortBase, "hub-port-base", 9100, "Hub output base port; channel N is published on base+N")
	flag.IntVar(&cfg.hubProtoOffset, "hub-protocol-offset", -1, "Protocol channel = user channel + offset (default -1)")
	flag.StringVar(&cfg.subscribeAddr, "subscribe", "", "Subscribe mode: tcp address from hub, e.g. 127.0.0.1:9101")
	flag.StringVar(&cfg.channelMapSpec, "channel-map", "", "Optional per-channel map user:proto (e.g. 1:0,2:1,3:4)")
	flag.StringVar(&cfg.metricsAddr, "metrics-addr", "", "Optional Prometheus metrics listen addr (e.g. 127.0.0.1:9910)")
	flag.BoolVar(&cfg.logJSON, "log-json", false, "Emit runtime logs as JSON lines (stderr)")
	flag.Parse()
	return cfg
}

func parseChannelMap(spec string) (map[int]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	out := make(map[int]int)
	usedProto := make(map[int]struct{})
	for _, tok := range strings.Split(spec, ",") {
		p := strings.TrimSpace(tok)
		if p == "" {
			continue
		}
		parts := strings.Split(p, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid channel-map token %q (expected user:proto)", p)
		}
		userCh, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, fmt.Errorf("invalid user channel in %q: %w", p, err)
		}
		protoCh, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("invalid protocol channel in %q: %w", p, err)
		}
		if userCh <= 0 {
			return nil, fmt.Errorf("user channel must be >=1 in %q", p)
		}
		if protoCh < 0 || protoCh > 7 {
			return nil, fmt.Errorf("protocol channel out of range in %q", p)
		}
		if _, ok := out[userCh]; ok {
			return nil, fmt.Errorf("duplicate user channel %d in channel-map", userCh)
		}
		if _, ok := usedProto[protoCh]; ok {
			return nil, fmt.Errorf("duplicate protocol channel %d in channel-map", protoCh)
		}
		out[userCh] = protoCh
		usedProto[protoCh] = struct{}{}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func newBridgeMetrics(mode string) *bridgeMetrics {
	return &bridgeMetrics{
		started:       time.Now(),
		mode:          mode,
		dropsByReason: make(map[string]uint64),
		framesByProto: make(map[uint32]uint64),
		bytesByProto:  make(map[uint32]uint64),
		syncByProto:   make(map[uint32]uint64),
		subsByProto:   make(map[uint32]int),
	}
}

func (m *bridgeMetrics) incReconnect() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.reconnects++
	m.mu.Unlock()
}

func (m *bridgeMetrics) addDrop(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.dropsByReason[reason]++
	m.mu.Unlock()
}

func (m *bridgeMetrics) addFrame(proto uint32, bytes int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.framesByProto[proto]++
	m.bytesByProto[proto] += uint64(bytes)
	m.mu.Unlock()
}

func (m *bridgeMetrics) addSync(proto uint32) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.syncByProto[proto]++
	m.mu.Unlock()
}

func (m *bridgeMetrics) addSubscriber(proto uint32, delta int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.subsByProto[proto] += delta
	if m.subsByProto[proto] < 0 {
		m.subsByProto[proto] = 0
	}
	m.mu.Unlock()
}

func (m *bridgeMetrics) writeProm(w io.Writer) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	_, _ = fmt.Fprintf(w, "# TYPE legacybridge_uptime_seconds gauge\nlegacybridge_uptime_seconds %.0f\n", time.Since(m.started).Seconds())
	_, _ = fmt.Fprintf(w, "# TYPE legacybridge_mode gauge\nlegacybridge_mode{mode=%q} 1\n", m.mode)
	_, _ = fmt.Fprintf(w, "# TYPE legacybridge_reconnect_total counter\nlegacybridge_reconnect_total %d\n", m.reconnects)

	_, _ = fmt.Fprintln(w, "# TYPE legacybridge_media_frames_total counter")
	for ch, v := range m.framesByProto {
		_, _ = fmt.Fprintf(w, "legacybridge_media_frames_total{proto_channel=%q} %d\n", strconv.Itoa(int(ch)), v)
	}

	_, _ = fmt.Fprintln(w, "# TYPE legacybridge_media_bytes_total counter")
	for ch, v := range m.bytesByProto {
		_, _ = fmt.Fprintf(w, "legacybridge_media_bytes_total{proto_channel=%q} %d\n", strconv.Itoa(int(ch)), v)
	}

	_, _ = fmt.Fprintln(w, "# TYPE legacybridge_sync_total counter")
	for ch, v := range m.syncByProto {
		_, _ = fmt.Fprintf(w, "legacybridge_sync_total{proto_channel=%q} %d\n", strconv.Itoa(int(ch)), v)
	}

	_, _ = fmt.Fprintln(w, "# TYPE legacybridge_subscribers gauge")
	for ch, v := range m.subsByProto {
		_, _ = fmt.Fprintf(w, "legacybridge_subscribers{proto_channel=%q} %d\n", strconv.Itoa(int(ch)), v)
	}

	_, _ = fmt.Fprintln(w, "# TYPE legacybridge_drop_total counter")
	for reason, v := range m.dropsByReason {
		_, _ = fmt.Fprintf(w, "legacybridge_drop_total{reason=%q} %d\n", reason, v)
	}
}

func startMetricsServer(ctx context.Context, addr string, m *bridgeMetrics, logger *log.Logger, verbose bool) *http.Server {
	if strings.TrimSpace(addr) == "" || m == nil {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		m.writeProm(w)
	})
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	go func() {
		if verbose {
			logger.Printf("metrics server listening on http://%s/metrics", addr)
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) && verbose {
			logger.Printf("metrics server error: %v", err)
		}
	}()
	return srv
}

func parseHubMappings(chSpec string, protoOffset, portBase int, chMap map[int]int) ([]hubChannel, error) {
	raw := strings.Split(chSpec, ",")
	seenUser := make(map[int]struct{})
	seenProto := make(map[int]struct{})
	mappings := make([]hubChannel, 0, len(raw))

	for _, tok := range raw {
		v := strings.TrimSpace(tok)
		if v == "" {
			continue
		}
		userCh, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid hub channel %q: %w", v, err)
		}
		if userCh <= 0 {
			return nil, fmt.Errorf("hub channel must be >= 1: %d", userCh)
		}
		if _, ok := seenUser[userCh]; ok {
			continue
		}
		protoCh := userCh + protoOffset
		if chMap != nil {
			if mapped, ok := chMap[userCh]; ok {
				protoCh = mapped
			}
		}
		if protoCh < 0 || protoCh > 7 {
			return nil, fmt.Errorf("hub channel %d maps to invalid protocol channel %d", userCh, protoCh)
		}
		if _, ok := seenProto[protoCh]; ok {
			return nil, fmt.Errorf("duplicate protocol channel %d in hub mapping", protoCh)
		}
		seenUser[userCh] = struct{}{}
		seenProto[protoCh] = struct{}{}
		mappings = append(mappings, hubChannel{
			user:  userCh,
			proto: uint32(protoCh),
			port:  portBase + userCh,
		})
	}
	if len(mappings) == 0 {
		return nil, fmt.Errorf("empty hub channel set")
	}
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].user < mappings[j].user })
	return mappings, nil
}

func runSubscribe(ctx context.Context, addr string, out io.Writer, reconnect time.Duration, logger *log.Logger, verbose bool, m *bridgeMetrics) error {
	dialer := net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	for {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			if m != nil {
				m.incReconnect()
				m.addDrop("subscribe_connect_error")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(reconnect):
				continue
			}
		}
		if verbose {
			logger.Printf("subscribe connected to %s", addr)
		}
		_, copyErr := io.Copy(out, conn)
		_ = conn.Close()
		if copyErr == nil {
			continue
		}
		if m != nil {
			m.incReconnect()
			m.addDrop("subscribe_copy_error")
		}
		if isBrokenPipe(copyErr) {
			return nil
		}
		if verbose {
			logger.Printf("subscribe stream ended: %v", copyErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconnect):
		}
	}
}

func (p *hubPublisher) acceptLoop(ctx context.Context, logger *log.Logger, verbose bool) {
	tcpLn, _ := p.ln.(*net.TCPListener)
	for {
		if tcpLn != nil {
			_ = tcpLn.SetDeadline(time.Now().Add(time.Second))
		}
		conn, err := p.ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				select {
				case <-ctx.Done():
					return
				default:
				}
				continue
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			if verbose {
				logger.Printf("hub ch=%d accept error: %v", p.ch.user, err)
			}
			continue
		}
		p.mu.Lock()
		p.conns[conn] = struct{}{}
		boot := append([]byte(nil), p.boot...)
		p.mu.Unlock()
		go p.watchDisconnect(conn)
		if p.m != nil {
			p.m.addSubscriber(p.ch.proto, 1)
		}
		if len(boot) > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(1200 * time.Millisecond))
			if _, err := conn.Write(boot); err != nil {
				_ = conn.Close()
				p.removeConn(conn, "subscriber_bootstrap_write_error")
				continue
			}
		}
		if verbose {
			logger.Printf("hub ch=%d subscriber connected from %s", p.ch.user, conn.RemoteAddr())
		}
	}
}

func (p *hubPublisher) watchDisconnect(conn net.Conn) {
	_, _ = io.Copy(io.Discard, conn)
	p.removeConn(conn, "")
}

func (p *hubPublisher) removeConn(conn net.Conn, dropReason string) {
	_ = conn.Close()
	p.mu.Lock()
	if _, ok := p.conns[conn]; !ok {
		p.mu.Unlock()
		return
	}
	delete(p.conns, conn)
	p.mu.Unlock()
	if p.m != nil {
		p.m.addSubscriber(p.ch.proto, -1)
		if dropReason != "" {
			p.m.addDrop(dropReason)
		}
	}
}

func (p *hubPublisher) publish(payload []byte, keyframe bool) {
	p.mu.Lock()
	if keyframe {
		p.boot = append(p.boot[:0], payload...)
	}
	conns := make([]net.Conn, 0, len(p.conns))
	for conn := range p.conns {
		conns = append(conns, conn)
	}
	p.mu.Unlock()

	var dead []net.Conn
	for _, conn := range conns {
		_ = conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
		if _, err := conn.Write(payload); err != nil {
			_ = conn.Close()
			dead = append(dead, conn)
		}
	}

	if len(dead) == 0 {
		return
	}
	for _, conn := range dead {
		p.removeConn(conn, "subscriber_write_error")
	}
}

func (p *hubPublisher) close() {
	_ = p.ln.Close()
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for conn := range p.conns {
		conns = append(conns, conn)
	}
	p.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
		p.removeConn(conn, "")
	}
}

func (c *client) makeHubPublishers(ctx context.Context, mappings []hubChannel) (map[uint32]*hubPublisher, error) {
	out := make(map[uint32]*hubPublisher, len(mappings))
	for _, m := range mappings {
		addr := net.JoinHostPort(c.cfg.hubBind, strconv.Itoa(m.port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, p := range out {
				p.close()
			}
			return nil, fmt.Errorf("hub listen channel=%d addr=%s: %w", m.user, addr, err)
		}
		p := &hubPublisher{
			ch:    m,
			ln:    ln,
			conns: make(map[net.Conn]struct{}),
			m:     c.metrics,
		}
		out[m.proto] = p
		go p.acceptLoop(ctx, c.logger, c.cfg.verbose)
		if c.cfg.verbose {
			c.logger.Printf("hub publish user_ch=%d proto_ch=%d on tcp://%s", m.user, m.proto, addr)
		}
	}
	return out, nil
}

func (c *client) closeHubPublishers(pubs map[uint32]*hubPublisher) {
	for _, p := range pubs {
		p.close()
	}
}

func (c *client) runHub(ctx context.Context) error {
	mappings, err := parseHubMappings(c.cfg.hubChannels, c.cfg.hubProtoOffset, c.cfg.hubPortBase, c.cfg.channelMap)
	if err != nil {
		return err
	}
	if c.cfg.verbose {
		c.logger.Printf("hub mode enabled: channels=%s bind=%s base_port=%d proto_offset=%d",
			c.cfg.hubChannels, c.cfg.hubBind, c.cfg.hubPortBase, c.cfg.hubProtoOffset)
	}
	pubs, err := c.makeHubPublishers(ctx, mappings)
	if err != nil {
		return err
	}
	defer c.closeHubPublishers(pubs)

	for {
		err := c.runHubOnce(ctx, mappings, pubs)
		if err == nil || errors.Is(err, context.Canceled) {
			return err
		}
		c.metrics.incReconnect()
		if c.cfg.verbose {
			c.logger.Printf("hub session ended: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.cfg.reconnect):
		}
	}
}

func (c *client) runHubOnce(ctx context.Context, mappings []hubChannel, pubs map[uint32]*hubPublisher) error {
	if err := c.connect(); err != nil {
		return err
	}
	defer c.close()
	if err := c.openDiag(); err != nil {
		return err
	}
	defer c.closeDiag()

	loginReply, err := c.login()
	if err != nil {
		return err
	}
	if c.cfg.verbose {
		c.logger.Printf("hub login ok: %s", compactXML(loginReply.payload))
	}

	drainDone := make(chan error, 1)
	go func() {
		drainDone <- c.drainCmd(ctx)
	}()

	if err := c.bootstrap(); err != nil {
		return err
	}

	for _, m := range mappings {
		if err := c.sendStreamChRequest(int(m.proto)); err != nil {
			return err
		}
	}

	socketID, err := c.readSocketID()
	if err != nil {
		return err
	}
	if c.cfg.verbose {
		c.logger.Printf("hub socket_id=%d", socketID)
	}
	for _, m := range mappings {
		if err := c.openChannel(int(m.proto), socketID); err != nil {
			return err
		}
	}

	keepDone := make(chan error, 1)
	go func() {
		keepDone <- c.keepAliveLoop(ctx)
	}()

	streamErr := c.streamHubMedia(ctx, pubs)

	select {
	case err := <-keepDone:
		if err != nil && !errors.Is(err, context.Canceled) && c.cfg.verbose {
			c.logger.Printf("hub keepalive: %v", err)
		}
	default:
	}

	select {
	case err := <-drainDone:
		if err != nil && !errors.Is(err, context.Canceled) && c.cfg.verbose {
			c.logger.Printf("hub cmd drain: %v", err)
		}
	default:
	}

	return streamErr
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
		c.metrics.incReconnect()
		if c.cfg.verbose {
			c.logger.Printf("stream ended: %v", err)
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
		return fmt.Errorf("protocol-channel out of range: %d", protoCh)
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

	// As seen in Python/captures: send stream_ch_request BEFORE reading media socket_id, otherwise DVR does not start streaming.
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
	if c.cfg.channelMap != nil {
		if mapped, ok := c.cfg.channelMap[c.cfg.channel]; ok {
			return mapped
		}
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

// sendStreamChRequest sends stream_ch_request (as in Python/captures) before reading media socket_id.
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
			// Initial ACK as in captures (client->server seq=800, payload socket_id)
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
	// stream_ch already sent in sendStreamChRequest; only open_channel and open_audio here

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

func (c *client) streamHubMedia(ctx context.Context, pubs map[uint32]*hubPublisher) error {
	pending := make([]byte, 0, 256*1024)
	tmp := make([]byte, 64*1024)
	synced := make(map[uint32]bool, len(pubs))
	openFrame := make(map[uint32]bool, len(pubs))

	for {
		if err := c.dataConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		n, err := c.dataReader.Read(tmp)
		if n > 0 {
			pending = append(pending, tmp[:n]...)
			consumed, procErr := c.processHubMediaFrames(pending, pubs, synced, openFrame)
			if procErr != nil {
				return procErr
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

func (c *client) processHubMediaFrames(pending []byte, pubs map[uint32]*hubPublisher, synced map[uint32]bool, openFrame map[uint32]bool) (int, error) {
	i := 0
	for i+mediaHeaderLen <= len(pending) {
		if !bytes.Equal(pending[i:i+4], magic) {
			j := bytes.Index(pending[i+1:], magic)
			if j < 0 {
				if len(pending) > 3 {
					return len(pending) - 3, nil
				}
				return 0, nil
			}
			i += j + 1
			continue
		}
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
		frameType := be32le(frameBytes[8:12])
		payload := frameBytes[mediaHeaderLen:]

		// ACK every media packet to keep DVR synchronized.
		_ = c.writeData(buildFrame(cmd, 2, mediaAckFlag, mediaAckSession, 0, nil))

		pub, ok := pubs[cmd]
		if !ok {
			i += frameLen
			continue
		}
		if c.cfg.strictFrameType {
			if frameType != 0 && frameType != 1 && frameType != 2 && frameType != 800 {
				c.metrics.addDrop("hub_invalid_frame_type")
				i += frameLen
				continue
			}
		}
		if payloadLen == 0 {
			i += frameLen
			continue
		}
		if frameType == 2 {
			if !c.cfg.includeSeq2 {
				c.metrics.addDrop("hub_ignore_type2")
				i += frameLen
				continue
			}
			if !synced[cmd] || !openFrame[cmd] {
				c.metrics.addDrop("hub_wait_sync_cont")
				i += frameLen
				continue
			}
			chunk := c.continuationChunk(payload)
			if len(chunk) == 0 {
				c.metrics.addDrop("hub_empty_cont")
				i += frameLen
				continue
			}
			c.metrics.addFrame(cmd, len(chunk))
			pub.publish(chunk, false)
			i += frameLen
			continue
		}
		if frameType != 0 && frameType != 1 {
			c.metrics.addDrop("hub_non_video_frame_type")
			i += frameLen
			continue
		}

		start := startcodeOffset(payload)
		if start < 0 || start >= len(payload) {
			c.metrics.addDrop("hub_no_startcode")
			i += frameLen
			continue
		}
		chunk := payload[start:]
		if len(chunk) == 0 {
			c.metrics.addDrop("hub_empty_chunk")
			i += frameLen
			continue
		}
		if !synced[cmd] {
			if frameType != 0 || !isKeyframeOrConfig(chunk) {
				c.metrics.addDrop("hub_wait_sync")
				i += frameLen
				continue
			}
			synced[cmd] = true
			c.metrics.addSync(cmd)
			if c.cfg.verbose {
				c.logger.Printf("hub sync acquired user_ch=%d proto_ch=%d", pub.ch.user, pub.ch.proto)
			}
		}
		c.metrics.addFrame(cmd, len(chunk))
		pub.publish(chunk, frameType == 0)
		openFrame[cmd] = true
		i += frameLen
	}
	return i, nil
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
		// Port 6002 (media) uses an extended 44-byte header:
		// - first 32 bytes are standard frame header
		// - plus 12 metadata bytes
		// - payload length at offset 40
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
		frameType := be32le(frameBytes[8:12]) // in DLLs this is frame_type (0/1/2), not a global sequence counter
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

		// ACK each media packet (seq=2, flag=2) as observed in client->server captures.
		_ = c.writeData(buildFrame(cmd, 2, mediaAckFlag, mediaAckSession, 0, nil))
		c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "recv")

		if c.cfg.strictFrameType {
			if frameType != 0 && frameType != 1 && frameType != 2 && frameType != 800 {
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_invalid_frame_type")
				c.metrics.addDrop("single_invalid_frame_type")
				i += frameLen
				continue
			}
		}

		if cmd != protoCh {
			c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_other_channel")
			c.metrics.addDrop("single_other_channel")
			i += frameLen
			continue
		}

		// Empty packets = DVR keepalive/heartbeat.
		if payloadLen == 0 {
			c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "keepalive_or_empty")
			i += frameLen
			continue
		}
		switch state {
		case stateSearchingConfig:
			// In DLL decoder logic, initial sync happens on frame_type=0 (I-frame/config start).
			if frameType != 0 {
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "wait_sync_type0")
				c.metrics.addDrop("single_wait_sync_type0")
				i += frameLen
				continue
			}
			start := startcodeOffset(payload)
			if start < 0 || start >= len(payload) {
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "wait_sync_no_startcode")
				c.metrics.addDrop("single_wait_sync_no_startcode")
				i += frameLen
				continue
			}
			toOut := payload[start:]
			if !isKeyframeOrConfig(toOut) {
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "wait_sync_not_keyframe")
				c.metrics.addDrop("single_wait_sync_not_keyframe")
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
				c.metrics.addFrame(cmd, len(toOut))
				c.metrics.addSync(cmd)
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "write_sync")
			}
			c.haveOpenFrame = true
			c.lastWasIDRStart = true
			state = stateStreaming
		case stateStreaming:
			switch frameType {
			case 0, 1:
				// New frame: payload starts with a 12-byte prefix and then Annex-B.
				start := startcodeOffset(payload)
				if start < 0 || start >= len(payload) {
					c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_start_no_startcode")
					c.metrics.addDrop("single_drop_start_no_startcode")
					i += frameLen
					continue
				}
				toOut := payload[start:]
				if len(toOut) == 0 {
					c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_empty_start")
					c.metrics.addDrop("single_drop_empty_start")
					i += frameLen
					continue
				}
				if _, err := out.Write(toOut); err != nil {
					return i, state, err
				}
				c.metrics.addFrame(cmd, len(toOut))
				c.haveOpenFrame = true
				c.lastWasIDRStart = frameType == 0
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "write_start")
			case 2:
				if !c.cfg.includeSeq2 {
					c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "ignore_type2")
					break
				}
				if !c.haveOpenFrame {
					c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_cont_without_start")
					c.metrics.addDrop("single_drop_cont_without_start")
					break
				}
				toOut := c.continuationChunk(payload)
				if len(toOut) == 0 {
					c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_empty_cont")
					c.metrics.addDrop("single_drop_empty_cont")
					break
				}
				if _, err := out.Write(toOut); err != nil {
					return i, state, err
				}
				c.metrics.addFrame(cmd, len(toOut))
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "write_cont")
			case 800:
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "ignore_socket_id")
			default:
				c.writeDiagMedia(cmd, frameType, flag, session, seqID, payload, "drop_unknown")
				c.metrics.addDrop("single_drop_unknown")
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

// writeData sends a frame on media connection (e.g. ACK seq=2). Required to keep DVR synchronized.
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

// extractDumpMedia44 extracts H264 from NEW legacy media dump format:
// 44-byte header (32 standard + 12 metadata) and payload_len at offset 40.
func extractDumpMedia44(src []byte, out io.Writer) (int, error) {
	i := 0
	written := 0
	synced := false
	streamCmd := uint32(0xffffffff)

	// First media frame may be socket_id with standard 32-byte header.
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
			// First NAL packet: strip 12-byte prefix and search Annex-B start code
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
			// Continuation (seq=2 with payload): captures show same 12-byte prefix as seq=1
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

// hasAnnexBStart reports whether buf starts with an Annex-B start code (4 or 3 bytes) and returns its length.
func hasAnnexBStart(buf []byte) (bool, int) {
	if len(buf) >= 4 && buf[0] == 0 && buf[1] == 0 && buf[2] == 0 && buf[3] == 1 {
		return true, 4
	}
	if len(buf) >= 3 && buf[0] == 0 && buf[1] == 0 && buf[2] == 1 {
		return true, 3
	}
	return false, 0
}

// startcodeOffset searches first Annex-B start code followed by a valid NAL byte (type 0-23).
// Returns start code index in payload, or -1.
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

func (c *client) continuationChunk(payload []byte) []byte {
	off := mediaPrefixLen
	if c.cfg.continuation0 {
		off = 0
	}
	if off >= len(payload) {
		return nil
	}
	chunk := payload[off:]
	if c.cfg.continuationSkip > 0 {
		if c.cfg.continuationSkip >= len(chunk) {
			return nil
		}
		chunk = chunk[c.cfg.continuationSkip:]
	}
	return chunk
}

var annexBStartCode = []byte{0x00, 0x00, 0x00, 0x01}

// firstNALType returns first NAL type (0-23) in Annex-B data, or -1.
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

// isKeyframeOrConfig: true when first NAL is SPS(7), PPS(8), or IDR(5). Used to avoid starting from a P-frame.
func isKeyframeOrConfig(annexB []byte) bool {
	t := firstNALType(annexB)
	return t == 5 || t == 7 || t == 8
}

// avccToAnnexB converts AVCC payload ([4-byte length][NAL]...) to Annex-B.
// be = true: big-endian length (MP4); be = false: little-endian.
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
