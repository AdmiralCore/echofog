package scenarios

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// fakeClientIP is the address every tapped client has in the pcap.
var fakeClientIP = netip.MustParseAddr("10.0.0.2")

const (
	// tapMSS is the segment size the tap shows: an Ethernet path.
	tapMSS = 1460
	// tapOneWay is the one-way delay between the observer and the server by default: a path of
	// 20 ms round trip. The observer sits at the border of the client, which is next to it.
	tapOneWay = 10 * time.Millisecond
)

type tapOption func(*tapConfig)

type tapConfig struct{ oneWay time.Duration }

// withOneWay sets the one-way delay between the observer and the server.
func withOneWay(d time.Duration) tapOption { return func(c *tapConfig) { c.oneWay = d } }

func tapSettings(opts []tapOption) tapConfig {
	c := tapConfig{oneWay: tapOneWay}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// delayLine runs functions in order, each no earlier than its due time.
type delayLine struct {
	ch   chan delayed
	done chan struct{}
}

type delayed struct {
	due time.Time
	fn  func()
}

func newDelayLine() *delayLine {
	d := &delayLine{ch: make(chan delayed, 4096), done: make(chan struct{})}
	go func() {
		defer close(d.done)
		for it := range d.ch {
			if w := time.Until(it.due); w > 0 {
				time.Sleep(w)
			}
			it.fn()
		}
	}()
	return d
}

func (d *delayLine) add(after time.Duration, fn func()) {
	d.ch <- delayed{time.Now().Add(after), fn}
}

// stop lets the queued functions run and waits for the last.
func (d *delayLine) stop() { close(d.ch); <-d.done }

// TCPTap is a TCP relay between a client and a server on loopback that writes the pcap of what it
// relays, with a fake client address (10.0.0.2, the real port of the client) and a fake server
// address.
//
// The observer is at the border of the client; the server is one one-way delay away. The pcap shows
// the handshake (SYN at once, SYN-ACK and ACK a round trip later), the data of the client when it
// is sent and the data of the server one delay after the server wrote it, in segments of at most
// 1460 bytes with sequence and acknowledgement numbers, the ACK of the server for the data of the
// client a round trip after it, the ACK of the client for the data of the server at once, and the
// FINs. Bytes reach the server one delay after they are seen and the client at the moment the pcap
// shows them. Time stamps are real.
type TCPTap struct {
	ln     net.Listener
	target string
	fake   netip.AddrPort
	pw     *PcapWriter
	cfg    tapConfig

	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// NewTCPTap listens on a loopback port and relays every connection to target.
func NewTCPTap(target string, fakeServer netip.AddrPort, pw *PcapWriter, opts ...tapOption) (*TCPTap, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t := &TCPTap{ln: ln, target: target, fake: fakeServer, pw: pw, cfg: tapSettings(opts), conns: map[net.Conn]struct{}{}}
	t.wg.Add(1)
	go t.accept()
	return t, nil
}

// Addr is the loopback address clients dial.
func (t *TCPTap) Addr() string { return t.ln.Addr().String() }

// Close stops the relay and ends the connections it holds.
func (t *TCPTap) Close() error {
	t.mu.Lock()
	t.closed = true
	for c := range t.conns {
		_ = c.Close()
	}
	t.mu.Unlock()
	err := t.ln.Close()
	t.wg.Wait()
	return err
}

func (t *TCPTap) track(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.conns[c] = struct{}{}
	return true
}

func (t *TCPTap) accept() {
	defer t.wg.Done()
	for {
		c, err := t.ln.Accept()
		if err != nil {
			return
		}
		if !t.track(c) {
			_ = c.Close()
			return
		}
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.relay(c)
		}()
	}
}

// tcpState numbers the segments of one relayed connection.
type tcpState struct {
	mu       sync.Mutex
	pw       *PcapWriter
	cli, srv netip.AddrPort
	cliSeq   uint32 // next sequence number the client sends
	srvSeq   uint32
}

func randSeq() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:]) >> 1
}

func (s *tcpState) syn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.pw.TCP(time.Now(), s.cli, s.srv, s.cliSeq, 0, tcpSYN, nil)
	s.cliSeq++
}

func (s *tcpState) synAck() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.pw.TCP(time.Now(), s.srv, s.cli, s.srvSeq, s.cliSeq, tcpSYN|tcpACK, nil)
	s.srvSeq++
	_ = s.pw.TCP(time.Now(), s.cli, s.srv, s.cliSeq, s.srvSeq, tcpACK, nil)
}

// sides returns the addresses and sequence counters of the sender and of the receiver.
func (s *tcpState) sides(fromClient bool) (from, to netip.AddrPort, seq, ack *uint32) {
	if fromClient {
		return s.cli, s.srv, &s.cliSeq, &s.srvSeq
	}
	return s.srv, s.cli, &s.srvSeq, &s.cliSeq
}

// send writes p in segments from one side and returns the sequence number after it.
func (s *tcpState) send(fromClient bool, p []byte) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	from, to, seq, ack := s.sides(fromClient)
	for len(p) > 0 {
		n := min(len(p), tapMSS)
		flags := uint8(tcpACK)
		if n == len(p) {
			flags |= tcpPSH
		}
		_ = s.pw.TCP(time.Now(), from, to, *seq, *ack, flags, p[:n])
		*seq += uint32(n)
		p = p[n:]
	}
	return *seq
}

// ack writes the pure ACK that the receiver of the data of fromClient sends for it, up to end.
func (s *tcpState) ack(fromClient bool, end uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from, to, seq, _ := s.sides(!fromClient)
	_ = s.pw.TCP(time.Now(), from, to, *seq, end, tcpACK, nil)
}

// fin writes the FIN of one side and returns the sequence number after it.
func (s *tcpState) fin(fromClient bool) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	from, to, seq, ack := s.sides(fromClient)
	_ = s.pw.TCP(time.Now(), from, to, *seq, *ack, tcpFIN|tcpACK, nil)
	*seq++
	return *seq
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func (t *TCPTap) relay(c net.Conn) {
	defer c.Close()
	ra, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	d := t.cfg.oneWay
	st := &tcpState{pw: t.pw, cli: netip.AddrPortFrom(fakeClientIP, uint16(ra.Port)), srv: t.fake, cliSeq: randSeq(), srvSeq: randSeq()}
	st.syn()
	up, err := net.DialTimeout("tcp", t.target, 3*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	if !t.track(up) {
		return
	}
	// the first bytes of the client wait in the kernel until the handshake has had its round trip
	time.Sleep(2 * d)
	st.synAck()

	toServer, ackLine, toClient := newDelayLine(), newDelayLine(), newDelayLine()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // the bytes of the client: seen at once, at the server one delay later
		defer wg.Done()
		buf := make([]byte, 16<<10)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				p := append([]byte(nil), buf[:n]...)
				end := st.send(true, p)
				toServer.add(d, func() { _, _ = up.Write(p) })
				ackLine.add(2*d, func() { st.ack(true, end) })
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					end := st.fin(true)
					toServer.add(d, func() { closeWrite(up) })
					ackLine.add(2*d, func() { st.ack(true, end) })
				}
				return
			}
		}
	}()
	go func() { // the bytes of the server: seen, and at the client, one delay after it wrote them
		defer wg.Done()
		buf := make([]byte, 16<<10)
		for {
			n, err := up.Read(buf)
			if n > 0 {
				p := append([]byte(nil), buf[:n]...)
				toClient.add(d, func() {
					end := st.send(false, p)
					st.ack(false, end)
					_, _ = c.Write(p)
				})
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					toClient.add(d, func() {
						end := st.fin(false)
						st.ack(false, end)
						closeWrite(c)
					})
				}
				return
			}
		}
	}()
	wg.Wait()
	toServer.stop()
	ackLine.stop()
	toClient.stop()
}

// UDPTap is the UDP counterpart: one upstream socket per client address, datagrams written as they
// were with the same delay model, the first datagram of the first client kept for the prober.
type UDPTap struct {
	conn   *net.UDPConn
	target *net.UDPAddr
	fake   netip.AddrPort
	pw     *PcapWriter
	cfg    tapConfig
	fwd    *delayLine // client to server
	back   *delayLine // server to client

	wg      sync.WaitGroup
	mu      sync.Mutex
	clients map[string]*net.UDPConn
	first   []byte
	closed  bool
}

// NewUDPTap listens on a loopback UDP port and relays datagrams to target.
func NewUDPTap(target string, fakeServer netip.AddrPort, pw *PcapWriter, opts ...tapOption) (*UDPTap, error) {
	ta, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	t := &UDPTap{conn: conn, target: ta, fake: fakeServer, pw: pw, cfg: tapSettings(opts),
		fwd: newDelayLine(), back: newDelayLine(), clients: map[string]*net.UDPConn{}}
	t.wg.Add(1)
	go t.serve()
	return t, nil
}

// Addr is the loopback address clients dial.
func (t *UDPTap) Addr() string { return t.conn.LocalAddr().String() }

// FirstClientDatagram is the first datagram of 1200 bytes or more that a client sent through the
// tap (an Init or an Initial: a legitimate first flight, which the prober replays), or the first
// datagram when none was that large.
func (t *UDPTap) FirstClientDatagram() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.first...)
}

// Close stops the relay.
func (t *UDPTap) Close() error {
	t.mu.Lock()
	t.closed = true
	for _, c := range t.clients {
		_ = c.Close()
	}
	t.mu.Unlock()
	err := t.conn.Close()
	t.wg.Wait()
	t.fwd.stop()
	t.back.stop()
	return err
}

func (t *UDPTap) serve() {
	defer t.wg.Done()
	buf := make([]byte, 64<<10)
	for {
		n, from, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt := append([]byte(nil), buf[:n]...)
		fakeCli := netip.AddrPortFrom(fakeClientIP, uint16(from.Port))
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return
		}
		if t.first == nil || (len(t.first) < 1200 && len(pkt) >= 1200) {
			t.first = pkt
		}
		up, ok := t.clients[from.String()]
		if !ok {
			up, err = net.DialUDP("udp", nil, t.target)
			if err != nil {
				t.mu.Unlock()
				continue
			}
			t.clients[from.String()] = up
			t.wg.Add(1)
			go t.serveBack(up, from, fakeCli)
		}
		t.mu.Unlock()
		_ = t.pw.UDP(time.Now(), fakeCli, t.fake, pkt)
		t.fwd.add(t.cfg.oneWay, func() { _, _ = up.Write(pkt) })
	}
}

// serveBack carries the datagrams of the server to one client.
func (t *UDPTap) serveBack(up *net.UDPConn, client *net.UDPAddr, fakeCli netip.AddrPort) {
	defer t.wg.Done()
	buf := make([]byte, 64<<10)
	for {
		n, err := up.Read(buf)
		if err != nil {
			return
		}
		p := append([]byte(nil), buf[:n]...)
		t.back.add(t.cfg.oneWay, func() {
			_ = t.pw.UDP(time.Now(), t.fake, fakeCli, p)
			_, _ = t.conn.WriteToUDP(p, client)
		})
	}
}
