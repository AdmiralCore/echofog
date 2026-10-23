package exit_gateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// udpIdleTimeout closes a relay nobody used for a while (QUIC, games, calls keep it alive).
// udpDNSIdleTimeout is the same for a relay that only ever sent DNS queries: an answer comes in
// milliseconds, and every lookup a computer makes opens a session of its own, so keeping them two
// minutes used up the peer's sessions while browsing. Variables so tests can shorten them.
var (
	udpIdleTimeout    = 2 * time.Minute
	udpDNSIdleTimeout = 15 * time.Second
	udpIdleCheck      = 5 * time.Second
)

// udpEvictIdle: when a peer is out of sessions, its least recently used one that moved nothing for
// this long is closed for the new one, as a NAT box does, instead of refusing the new one.
var udpEvictIdle = 5 * time.Second

const (
	maxDatagram = 65535
	// maxUDPTargets bounds the per-association resolution cache.
	maxUDPTargets = 1024
)

var ErrBadDatagram = errors.New("malformed UDP relay datagram")

// encodeDatagram frames one datagram as [2-byte address length][address][payload].
func encodeDatagram(addr string, payload []byte) ([]byte, error) {
	if len(addr) == 0 || len(addr) > maxTargetLen || len(payload) > maxDatagram {
		return nil, ErrBadDatagram
	}
	out := make([]byte, 2+len(addr)+len(payload))
	binary.BigEndian.PutUint16(out, uint16(len(addr)))
	copy(out[2:], addr)
	copy(out[2+len(addr):], payload)
	return out, nil
}

func decodeDatagram(frame []byte) (string, []byte, error) {
	if len(frame) < 2 {
		return "", nil, ErrBadDatagram
	}
	n := int(binary.BigEndian.Uint16(frame))
	if n == 0 || n > maxTargetLen || len(frame) < 2+n {
		return "", nil, ErrBadDatagram
	}
	return string(frame[2 : 2+n]), frame[2+n:], nil
}

// UDPSession is the client side of a UDP relay through an exit. Datagrams are addressed by
// "host:port"; host may be a domain, which the exit resolves, and replies come back labelled
// with the same "host:port" the client used.
type UDPSession struct {
	st     FramedStream
	mu     sync.Mutex
	closed bool

	// datagram mode (nil dg: everything rides the stream)
	dg    *dgramSender
	unsub func()
	frag  defrag
	in    chan udpPacket // from datagrams and from the stream
	ended chan struct{}  // the stream is gone; rerr says why
	rerr  error
	done  chan struct{} // closed by Close
	once  sync.Once
}

type udpPacket struct {
	data []byte
	addr string
}

// udpInQueue is how many received datagrams wait for the reader before new ones are dropped.
const udpInQueue = 512

// DialExitUDP opens a UDP relay on the exit peer.
func (d *P2PDialer) DialExitUDP(ctx context.Context) (*UDPSession, error) {
	if d == nil || d.open == nil {
		return nil, errNoStreamOpener
	}
	st, err := d.open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open stream to exit peer: %w", err)
	}
	var link udpLink
	if ds, ok := st.(datagramStream); ok {
		link = ds.udpLink(true)
	}
	var hello []byte
	if link != nil {
		hello = []byte{udpHelloDatagrams}
	}
	if err := st.WriteFrame(net_types.TypeExitUDP, hello); err != nil {
		st.Close()
		return nil, err
	}
	type reply struct {
		t   net_types.MessageType
		b   []byte
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		t, b, err := st.ReadFrame()
		ch <- reply{t, b, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			st.Close()
			return nil, r.err
		}
		if r.t != net_types.TypeExitConnectResult || len(r.b) < 1 {
			st.Close()
			return nil, ErrExitProtocol
		}
		switch r.b[0] {
		case resultOK:
			u := &UDPSession{st: st}
			if link != nil && len(r.b) >= 5 {
				if err := u.useDatagrams(link, binary.BigEndian.Uint32(r.b[1:5])); err != nil {
					st.Close()
					return nil, err
				}
			}
			return u, nil
		case resultPayment:
			st.Close()
			return nil, ErrPaymentRequired
		case resultBusy:
			st.Close()
			return nil, ErrExitBusy
		default:
			st.Close()
			return nil, ErrExitDenied
		}
	case <-ctx.Done():
		st.Close()
		return nil, ctx.Err()
	}
}

func (u *UDPSession) useDatagrams(link udpLink, id uint32) error {
	u.in = make(chan udpPacket, udpInQueue)
	u.ended = make(chan struct{})
	u.done = make(chan struct{})
	u.unsub = link.subscribe(id, u.onDatagram)
	if u.unsub == nil {
		return ErrExitProtocol // the exit handed out a session ID it already gave us
	}
	u.dg = &dgramSender{link: link, id: id}
	go u.readStream()
	return nil
}

// Datagrams reports whether the session's datagrams travel as QUIC datagrams.
func (u *UDPSession) Datagrams() bool { return u.dg != nil }

// onDatagram runs on the connection's datagram loop: it must not block.
func (u *UDPSession) onDatagram(b []byte) {
	f, ok := decodeUDPFragment(b)
	if !ok {
		return
	}
	data, ok := u.frag.feed(f)
	if !ok {
		return
	}
	select {
	case u.in <- udpPacket{data, f.addr}:
	default: // the reader is behind: drop, as a full socket buffer would
	}
}

// readStream feeds what still comes over the stream (datagrams too big for the path) to ReadFrom.
func (u *UDPSession) readStream() {
	defer close(u.ended)
	for {
		t, frame, err := u.st.ReadFrame()
		if err != nil {
			u.rerr = err
			return
		}
		if t != net_types.TypeExitUDP {
			continue
		}
		addr, payload, err := decodeDatagram(frame)
		if err != nil {
			continue
		}
		select {
		case u.in <- udpPacket{payload, addr}:
		case <-u.done:
			return
		}
	}
}

// WriteTo sends one datagram to addr ("host:port").
func (u *UDPSession) WriteTo(payload []byte, addr string) error {
	if len(addr) == 0 || len(addr) > maxTargetLen || len(payload) > maxDatagram {
		return ErrBadDatagram
	}
	u.mu.Lock()
	closed := u.closed
	u.mu.Unlock()
	if closed {
		return net.ErrClosed
	}
	if u.dg != nil && u.dg.send(addr, payload) {
		return nil
	}
	frame, err := encodeDatagram(addr, payload)
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return net.ErrClosed
	}
	return u.st.WriteFrame(net_types.TypeExitUDP, frame)
}

// ReadFrom blocks for the next datagram and the "host:port" it came from.
func (u *UDPSession) ReadFrom() ([]byte, string, error) {
	if u.dg != nil {
		select {
		case p := <-u.in:
			return p.data, p.addr, nil
		case <-u.ended:
			select {
			case p := <-u.in:
				return p.data, p.addr, nil
			default:
			}
			return nil, "", u.rerr
		}
	}
	for {
		t, frame, err := u.st.ReadFrame()
		if err != nil {
			return nil, "", err
		}
		if t != net_types.TypeExitUDP {
			continue
		}
		addr, payload, err := decodeDatagram(frame)
		if err != nil {
			continue
		}
		return payload, addr, nil
	}
}

func (u *UDPSession) Close() error {
	u.mu.Lock()
	u.closed = true
	unsub := u.unsub
	u.unsub = nil
	u.mu.Unlock()
	if unsub != nil {
		unsub()
	}
	if u.done != nil {
		u.once.Do(func() { close(u.done) })
	}
	return u.st.Close()
}

// HandleUDPFrom serves one UDP relay stream for a peer: datagrams from the stream go out of
// one exit UDP socket, replies go back labelled with the address the client asked for.
func (e *ExitService) HandleUDPFrom(ctx context.Context, st FramedStream, peer types.PeerID) {
	e.handleUDP(ctx, st, peer, nil)
}

// handleUDP is HandleUDPFrom with the client's request: a client that can take datagrams gets
// a session ID, and its datagrams travel outside the stream when the link is QUIC.
func (e *ExitService) handleUDP(ctx context.Context, st FramedStream, peer types.PeerID, hello []byte) {
	defer st.Close()
	if !e.serves(peer) {
		e.refuse(peer, RefuseDisabled)
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultDenied})
		return
	}
	if e.allowance(peer) == 0 {
		e.refuse(peer, e.paymentRefusal())
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultPayment})
		return
	}
	release, ok := e.acquire(peer, true)
	if !ok && e.udp.evictOldest(peer) {
		release, ok = e.acquire(peer, true)
	}
	if !ok {
		e.refuse(peer, RefuseBusy)
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultBusy})
		return
	}
	defer release()
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		e.refuse(peer, RefuseDial)
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultDialFailed})
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := newMeter(e, peer)
	defer m.close()
	p, unpace := e.paceFor(peer)
	defer unpace()
	relay := &udpRelay{svc: e, st: st, conn: conn, peer: peer, meter: m, pace: p, cancel: cancel,
		labels: make(map[string]string), resolved: make(map[string]*net.UDPAddr),
		pending: make(map[string][][]byte), failed: make(map[string]time.Time), release: release}
	e.udp.add(relay)
	defer e.udp.remove(relay)

	reply := []byte{resultOK}
	if len(hello) > 0 && hello[0]&udpHelloDatagrams != 0 {
		if ds, ok := st.(datagramStream); ok {
			if link := ds.udpLink(false); link != nil {
				relay.dgIn = make(chan udpPacket, udpInQueue)
				for tries := 0; tries < 8 && relay.dg == nil; tries++ {
					var b [4]byte
					_, _ = rand.Read(b[:])
					id := binary.BigEndian.Uint32(b[:])
					if unsub := link.subscribe(id, relay.onDatagram); unsub != nil {
						defer unsub()
						relay.dg = &dgramSender{link: link, id: id}
					}
				}
			}
		}
	}
	if relay.dg != nil {
		reply = binary.BigEndian.AppendUint32(reply, relay.dg.id)
	}
	if err := st.WriteFrame(net_types.TypeExitConnectResult, reply); err != nil {
		return
	}
	e.accepted.Add(1)

	relay.touch()
	go relay.watchIdle(ctx, cancel)
	go func() {
		relay.downstream()
		cancel()
	}()
	if relay.dg != nil {
		go relay.fromDatagrams(ctx)
	}
	relay.upstream(ctx)
}

const (
	// udpResolveRetry: a name that did not resolve is not looked up again before this.
	udpResolveRetry = 10 * time.Second
	// maxUDPResolving bounds the lookups one relay runs at once.
	maxUDPResolving = 64
)

type udpRelay struct {
	svc    *ExitService
	st     FramedStream
	conn   *net.UDPConn
	peer   types.PeerID
	meter  *meter
	pace   *pace // nil: not slowed down
	cancel context.CancelFunc

	dg   *dgramSender   // nil: replies ride the stream
	dgIn chan udpPacket // client datagrams, from the connection's datagram loop
	frag defrag

	mu       sync.Mutex
	labels   map[string]string       // "ip:port" actually used -> "host:port" the client asked for
	resolved map[string]*net.UDPAddr // "host:port" -> resolved destination
	pending  map[string][][]byte     // "host:port" being resolved -> datagrams waiting for it
	failed   map[string]time.Time    // "host:port" that did not resolve -> when
	lastUse  time.Time
	nonDNS   bool // it sent something else than DNS queries: the long idle timeout applies
	writeMu  sync.Mutex

	release func() // gives the stream slot back; safe to call more than once
}

// evict closes the relay and gives its slot back at once, for a new session of the same peer.
func (r *udpRelay) evict() {
	r.release()
	r.cancel()
}

func (r *udpRelay) idleFor() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.lastUse)
}

func (r *udpRelay) touch() {
	r.mu.Lock()
	r.lastUse = time.Now()
	r.mu.Unlock()
}

func (r *udpRelay) watchIdle(ctx context.Context, cancel context.CancelFunc) {
	t := time.NewTicker(udpIdleCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.release() // the slot is free now, not when the stream reader notices
			_ = r.conn.Close()
			_ = r.st.Close()
			return
		case <-t.C:
			r.mu.Lock()
			limit := udpIdleTimeout
			if !r.nonDNS {
				limit = udpDNSIdleTimeout
			}
			idle := time.Since(r.lastUse) > limit
			r.mu.Unlock()
			if idle {
				cancel()
			}
		}
	}
}

// account spends the peer's allowance for both directions; false means it is used up.
func (r *udpRelay) account(n int) bool {
	return r.meter.spend(n)
}

func (r *udpRelay) resolve(ctx context.Context, target string) (*net.UDPAddr, error) {
	r.mu.Lock()
	if a, ok := r.resolved[target]; ok {
		r.mu.Unlock()
		return a, nil
	}
	r.mu.Unlock()

	host, portStr, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return nil, ErrInvalidTarget
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 || r.svc.portDenied(port) {
		return nil, ErrInvalidTarget
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		rctx, cancel := context.WithTimeout(ctx, r.svc.dialTimeout)
		ips, err = r.svc.resolver(rctx, host)
		cancel()
		if err != nil || len(ips) == 0 {
			return nil, ErrExitDialFailed
		}
	}
	var dst *net.UDPAddr
	for _, ip := range ips {
		if r.svc.targetBlocked(ip) {
			continue
		}
		if ip.To4() != nil || dst == nil {
			dst = &net.UDPAddr{IP: ip, Port: port}
		}
		if ip.To4() != nil {
			break // the relay socket is dual-stack, but IPv4 is the safer default
		}
	}
	if dst == nil {
		return nil, ErrBlockedAddress
	}
	r.mu.Lock()
	if len(r.resolved) >= maxUDPTargets {
		r.resolved = make(map[string]*net.UDPAddr)
		r.labels = make(map[string]string)
	}
	r.resolved[target] = dst
	r.labels[dst.String()] = target
	r.mu.Unlock()
	return dst, nil
}

// send passes a client datagram on. A target not resolved yet is looked up off the caller's
// loop, its datagrams waiting for it, so one slow DNS answer does not hold up the others.
func (r *udpRelay) send(ctx context.Context, target string, payload []byte) {
	r.mu.Lock()
	if dst := r.resolved[target]; dst != nil {
		r.mu.Unlock()
		r.out(dst, payload)
		return
	}
	if t, ok := r.failed[target]; ok && time.Since(t) < udpResolveRetry {
		r.mu.Unlock()
		return // unreachable or private: drop silently like a lossy network would
	}
	if q, ok := r.pending[target]; ok {
		if len(q) < pendingUDP {
			r.pending[target] = append(q, payload)
		}
		r.mu.Unlock()
		return
	}
	if len(r.pending) >= maxUDPResolving {
		r.mu.Unlock()
		return
	}
	r.pending[target] = [][]byte{payload}
	r.mu.Unlock()
	go func() {
		dst, err := r.resolve(ctx, target)
		r.mu.Lock()
		q := r.pending[target]
		delete(r.pending, target)
		if err != nil {
			if len(r.failed) >= maxUDPTargets {
				r.failed = make(map[string]time.Time)
			}
			r.failed[target] = time.Now()
		}
		r.mu.Unlock()
		if err != nil {
			return
		}
		for _, p := range q {
			r.out(dst, p)
		}
	}()
}

func (r *udpRelay) out(dst *net.UDPAddr, payload []byte) {
	r.mu.Lock()
	r.lastUse = time.Now()
	if dst.Port != 53 {
		r.nonDNS = true
	}
	r.mu.Unlock()
	if !r.pace.allow(len(payload)) {
		return // over the fair share: dropped, as a congested link would
	}
	if !r.account(len(payload)) {
		r.cancel()
		return
	}
	_, _ = r.conn.WriteToUDP(payload, dst)
}

// upstream: client datagrams that come over the stream -> Internet.
func (r *udpRelay) upstream(ctx context.Context) {
	for {
		t, frame, err := r.st.ReadFrame()
		if err != nil {
			return
		}
		if t != net_types.TypeExitUDP {
			continue
		}
		target, payload, err := decodeDatagram(frame)
		if err != nil {
			continue
		}
		r.send(ctx, target, payload)
	}
}

// onDatagram runs on the connection's datagram loop: it must not block.
func (r *udpRelay) onDatagram(b []byte) {
	f, ok := decodeUDPFragment(b)
	if !ok {
		return
	}
	data, ok := r.frag.feed(f)
	if !ok {
		return
	}
	select {
	case r.dgIn <- udpPacket{data, f.addr}:
	default:
	}
}

// fromDatagrams: client datagrams that come as QUIC datagrams -> Internet.
func (r *udpRelay) fromDatagrams(ctx context.Context) {
	for {
		select {
		case p := <-r.dgIn:
			r.send(ctx, p.addr, p.data)
		case <-ctx.Done():
			return
		}
	}
}

// downstream: Internet replies -> client, labelled with the address the client used.
func (r *udpRelay) downstream() {
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			if udpReadTransient(err) {
				continue
			}
			return
		}
		key := (&net.UDPAddr{IP: from.IP, Port: from.Port}).String()
		r.mu.Lock()
		label, ok := r.labels[key]
		r.mu.Unlock()
		if !ok {
			continue // only answers from destinations the client contacted
		}
		r.touch()
		if !r.pace.allow(n) {
			continue
		}
		if !r.account(n) {
			return
		}
		if r.dg != nil && r.dg.send(label, buf[:n]) {
			continue
		}
		frame, err := encodeDatagram(label, buf[:n])
		if err != nil {
			continue
		}
		r.writeMu.Lock()
		err = r.st.WriteFrame(net_types.TypeExitUDP, frame)
		r.writeMu.Unlock()
		if err != nil {
			return
		}
	}
}

// udpRelays lists the UDP sessions the exit serves per peer, so a peer out of sessions can have
// its stalest one replaced.
type udpRelays struct {
	mu     sync.Mutex
	byPeer map[types.PeerID]map[*udpRelay]struct{}
}

func (u *udpRelays) add(r *udpRelay) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.byPeer == nil {
		u.byPeer = map[types.PeerID]map[*udpRelay]struct{}{}
	}
	if u.byPeer[r.peer] == nil {
		u.byPeer[r.peer] = map[*udpRelay]struct{}{}
	}
	u.byPeer[r.peer][r] = struct{}{}
}

func (u *udpRelays) remove(r *udpRelay) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if set := u.byPeer[r.peer]; set != nil {
		delete(set, r)
		if len(set) == 0 {
			delete(u.byPeer, r.peer)
		}
	}
}

// evictOldest closes the peer's least recently used session if it moved nothing for udpEvictIdle,
// and says whether it did.
func (u *udpRelays) evictOldest(peer types.PeerID) bool {
	u.mu.Lock()
	var oldest *udpRelay
	var idle time.Duration
	for r := range u.byPeer[peer] {
		if d := r.idleFor(); d > idle {
			oldest, idle = r, d
		}
	}
	if oldest == nil || idle < udpEvictIdle {
		u.mu.Unlock()
		return false
	}
	delete(u.byPeer[peer], oldest)
	u.mu.Unlock()
	oldest.evict()
	return true
}
