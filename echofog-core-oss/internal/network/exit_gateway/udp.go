package exit_gateway

import (
	"context"
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

const (
	// udpIdleTimeout closes a relay nobody used for a while (DNS, QUIC, games keep it alive).
	udpIdleTimeout = 2 * time.Minute
	maxDatagram    = 65535
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
}

// DialExitUDP opens a UDP relay on the exit peer.
func (d *P2PDialer) DialExitUDP(ctx context.Context) (*UDPSession, error) {
	if d == nil || d.open == nil {
		return nil, errNoStreamOpener
	}
	st, err := d.open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open stream to exit peer: %w", err)
	}
	if err := st.WriteFrame(net_types.TypeExitUDP, nil); err != nil {
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
			return &UDPSession{st: st}, nil
		case resultPayment:
			st.Close()
			return nil, ErrPaymentRequired
		default:
			st.Close()
			return nil, ErrExitDenied
		}
	case <-ctx.Done():
		st.Close()
		return nil, ctx.Err()
	}
}

// WriteTo sends one datagram to addr ("host:port").
func (u *UDPSession) WriteTo(payload []byte, addr string) error {
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
	u.mu.Unlock()
	return u.st.Close()
}

// HandleUDPFrom serves one UDP relay stream for a peer: datagrams from the stream go out of
// one exit UDP socket, replies go back labelled with the address the client asked for.
func (e *ExitService) HandleUDPFrom(ctx context.Context, st FramedStream, peer types.PeerID) {
	defer st.Close()
	if !e.enabled.Load() {
		e.refused.Add(1)
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultDenied})
		return
	}
	if e.allowance(peer) == 0 {
		e.refused.Add(1)
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultPayment})
		return
	}
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultDialFailed})
		return
	}
	defer conn.Close()
	if err := st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultOK}); err != nil {
		return
	}
	e.accepted.Add(1)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	relay := &udpRelay{svc: e, st: st, conn: conn, peer: peer, labels: make(map[string]string), resolved: make(map[string]*net.UDPAddr)}
	relay.touch()
	go relay.watchIdle(ctx, cancel)
	go func() {
		relay.downstream()
		cancel()
	}()
	relay.upstream(ctx)
}

type udpRelay struct {
	svc  *ExitService
	st   FramedStream
	conn *net.UDPConn
	peer types.PeerID

	mu       sync.Mutex
	labels   map[string]string       // "ip:port" actually used -> "host:port" the client asked for
	resolved map[string]*net.UDPAddr // "host:port" -> resolved destination
	lastUse  time.Time
	writeMu  sync.Mutex
}

func (r *udpRelay) touch() {
	r.mu.Lock()
	r.lastUse = time.Now()
	r.mu.Unlock()
}

func (r *udpRelay) watchIdle(ctx context.Context, cancel context.CancelFunc) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = r.conn.Close()
			_ = r.st.Close()
			return
		case <-t.C:
			r.mu.Lock()
			idle := time.Since(r.lastUse) > udpIdleTimeout
			r.mu.Unlock()
			if idle {
				cancel()
			}
		}
	}
}

// account spends the peer's allowance for both directions; false means it is used up.
func (r *udpRelay) account(n int) bool {
	return r.svc.consume(r.peer, int64(n))
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

// upstream: client datagrams -> Internet.
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
		dst, err := r.resolve(ctx, target)
		if err != nil {
			continue // unreachable or private: drop silently like a lossy network would
		}
		r.touch()
		if !r.account(len(payload)) {
			return
		}
		_, _ = r.conn.WriteToUDP(payload, dst)
	}
}

// downstream: Internet replies -> client, labelled with the address the client used.
func (r *udpRelay) downstream() {
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := r.conn.ReadFromUDP(buf)
		if err != nil {
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
		if !r.account(n) {
			return
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
