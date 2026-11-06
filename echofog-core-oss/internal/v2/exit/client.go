package exit

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// ErrClientClosed is returned by a closed Client.
var ErrClientClosed = errors.New("exit: client closed")

// ClientConfig configures a Client.
type ClientConfig struct {
	// OpenStream opens a stream of exit/2 on the session to the exit (required).
	OpenStream func(ctx context.Context) (*mux.Stream, error)
	// SendDatagram sends an application datagram on the session. Nil means the session cannot
	// carry datagrams, and UDP rides the stream.
	SendDatagram func(typ uint64, payload []byte) error
	// Shape is the shape profile of the exit's address (H1). Nil pads nothing.
	Shape *shape.Profile
	// Sleep waits out the H1 jitter; nil means time.Sleep.
	Sleep func(time.Duration)
	// Clock times the reassembly of EXIT_UDP fragments (AD13); nil means the system clock.
	Clock carrier.Clock
	// NoCamouflage says the exit's card has no camouflaged address: a failure to reach it is then
	// reported as ReasonNoCamouflage instead of ReasonNoPath.
	NoCamouflage bool
	// OnClose runs once when the client is closed (it releases the datagram port).
	OnClose func()
}

// Client is exit/2 on the client side: it turns the v1 exit core's tunnel calls (SOCKS5 entrance,
// TUN) into exit/2 streams and EXIT_UDP datagrams of one session to one exit. Dial it under the
// per-exit client identity (node.Runtime.ExitClient); the exit must never see the messenger one.
type Client struct {
	cfg    ClientConfig
	now    func() time.Time
	dialer *core.P2PDialer

	mu     sync.Mutex
	closed bool
	subs   map[uint32]*udpSub
}

// NewClient returns a client of one exit.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.OpenStream == nil {
		return nil, errors.New("exit: client needs OpenStream")
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	c := &Client{cfg: cfg, now: time.Now, subs: map[uint32]*udpSub{}}
	if cfg.Clock != nil {
		c.now = cfg.Clock.Now
	}
	c.dialer = core.NewP2PDialer(c.open)
	return c, nil
}

// DialExitStream implements core.StreamDialer: a TCP tunnel to target ("host:port"). The request
// goes out at once (fast open); the exit's refusal comes back from the first Read.
func (c *Client) DialExitStream(ctx context.Context, target string) (io.ReadWriteCloser, error) {
	if _, _, err := splitTarget(target); err != nil {
		return nil, err
	}
	return c.dialer.DialExitStream(ctx, target)
}

// DialExitUDP implements core.UDPDialer: a UDP tunnel, on EXIT_UDP datagrams when the session
// carries them and on UDP_PKT frames otherwise.
func (c *Client) DialExitUDP(ctx context.Context) (*core.UDPSession, error) {
	return c.dialer.DialExitUDP(ctx)
}

// Close ends the client's datagram subscriptions. Open tunnels belong to their callers.
func (c *Client) Close() error {
	c.mu.Lock()
	already := c.closed
	c.closed = true
	c.subs = map[uint32]*udpSub{}
	c.mu.Unlock()
	if !already && c.cfg.OnClose != nil {
		c.cfg.OnClose()
	}
	return nil
}

func (c *Client) open(ctx context.Context) (core.FramedStream, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, ErrClientClosed
	}
	st, err := c.cfg.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	cs := newClientStream(st, c.cfg.Shape, c.cfg.Sleep, nil)
	cs.client = c
	return cs, nil
}

// splitTarget validates a "host:port" target against the exit/2 bounds (services.md §7).
func splitTarget(target string) (string, uint32, error) {
	host, ps, err := net.SplitHostPort(target)
	if err != nil || host == "" || len(host) > maxHostLen {
		return "", 0, core.ErrInvalidTarget
	}
	port, err := strconv.ParseUint(ps, 10, 16)
	if err != nil || port == 0 {
		return "", 0, core.ErrInvalidTarget
	}
	return host, uint32(port), nil
}

// ---- datagrams

// udpSub is the receiving side of one UDP tunnel.
type udpSub struct {
	mu    sync.Mutex
	frags *reassembler
	fn    func([]byte)
}

// HandleDatagram feeds one EXIT_UDP datagram received from the exit. Datagrams of an unknown
// session, malformed ones and incomplete packets are dropped silently: the exit is not told.
func (c *Client) HandleDatagram(payload []byte) {
	f, err := ParseFragment(payload)
	if err != nil || f.Session == 0 || f.Session > 0xffffffff {
		return
	}
	c.mu.Lock()
	sub := c.subs[uint32(f.Session)]
	c.mu.Unlock()
	if sub == nil {
		return
	}
	sub.mu.Lock()
	addr, data, ok := sub.frags.feed(f)
	sub.mu.Unlock()
	if !ok {
		return
	}
	sub.fn(coreFragment(0, 1, addr, data))
}

// clientLink is the core.UDPLink of the datagram mode: it translates the v1 core's datagram
// frames to EXIT_UDP fragments and back, one tunnel per subscription.
type clientLink struct{ c *Client }

// Send translates one v1 frame (session ‖ pkt ‖ idx ‖ count ‖ addr_len(u16) ‖ addr ‖ data) to an
// EXIT_UDP datagram. The address travels in fragment 0 only.
func (l clientLink) Send(b []byte) error {
	limit := mux.MaxDatagram - 1 - 2 // v2 fragment 0 is at most 2 bytes bigger than the v1 frame
	if len(b) > limit {
		return &core.DatagramTooLargeError{Size: len(b), Max: limit}
	}
	if len(b) < core.UDPDgramHeader {
		return core.ErrBadDatagram
	}
	f := Fragment{
		Session: uint64(binary.BigEndian.Uint32(b)),
		Packet:  binary.BigEndian.Uint16(b[4:]),
		Index:   b[6],
		Count:   b[7],
	}
	n := int(binary.BigEndian.Uint16(b[8:]))
	if n == 0 || len(b) < core.UDPDgramHeader+n || f.Count == 0 || f.Count > MaxFragments || f.Index >= f.Count {
		return core.ErrBadDatagram
	}
	if f.Index == 0 {
		f.Addr = string(b[core.UDPDgramHeader : core.UDPDgramHeader+n])
		if len(f.Addr) > 254 || !utf8.ValidString(f.Addr) {
			return core.ErrBadDatagram
		}
	}
	f.Data = b[core.UDPDgramHeader+n:]
	out := f.Append(nil)
	if len(out) > mux.MaxDatagram-1 {
		return &core.DatagramTooLargeError{Size: len(b), Max: limit}
	}
	return l.c.cfg.SendDatagram(frame.DgramExitUDP, out)
}

// Subscribe registers fn for the datagrams of session id; it gets whole packets as v1 frames
// without the session id. Nil when the id is taken.
func (l clientLink) Subscribe(id uint32, fn func([]byte)) func() {
	c := l.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || id == 0 {
		return nil
	}
	if _, used := c.subs[id]; used {
		return nil
	}
	sub := &udpSub{frags: newReassembler(c.now), fn: fn}
	c.subs[id] = sub
	return func() {
		c.mu.Lock()
		if c.subs[id] == sub {
			delete(c.subs, id)
		}
		c.mu.Unlock()
	}
}

// coreFragment builds a v1 datagram frame without the session id:
// pkt ‖ idx ‖ count ‖ addr_len(u16) ‖ addr ‖ data.
func coreFragment(idx, count uint8, addr string, data []byte) []byte {
	out := make([]byte, 0, 6+len(addr)+len(data))
	out = binary.BigEndian.AppendUint16(out, 0)
	out = append(out, idx, count)
	out = binary.BigEndian.AppendUint16(out, uint16(len(addr)))
	out = append(out, addr...)
	return append(out, data...)
}

// ---- the framed stream of one tunnel

// wireStream is the part of *mux.Stream the client stream uses.
type wireStream interface {
	WriteFrame(typ uint64, flags byte, payload []byte) error
	ReadFrame() (frame.Frame, error)
	CloseWrite() error
	Close() error
}

var _ wireStream = (*mux.Stream)(nil)

type tunnelMode int

const (
	modeNew tunnelMode = iota
	modeTCP
	modeUDP
)

// clientStream is the core.FramedStream of one tunnel on the client side. The core writes v1
// frames (connect request, UDP request and datagrams) and the stream turns them into exit/2
// frames; DATA is padded for the first records of the direction (H1).
type clientStream struct {
	ws     wireStream
	client *Client

	wmu  sync.Mutex // serialises writers and guards pad, mode and req
	pad  *padder
	mode tunnelMode
	req  uint64

	buf []byte // the rest of a DATA frame that did not fit the reader's buffer
}

func newClientStream(ws wireStream, prof *shape.Profile, sleep func(time.Duration), rnd *mrand.Rand) *clientStream {
	return &clientStream{ws: ws, pad: newPadder(prof, sleep, rnd)}
}

// UDPLink implements core.DatagramStream: the datagram mode exists only when the session can
// send datagrams.
func (s *clientStream) UDPLink(bool) core.UDPLink {
	if s.client == nil || s.client.cfg.SendDatagram == nil {
		return nil
	}
	return clientLink{c: s.client}
}

// WriteFrame turns a v1 core frame into exit/2: TypeExitConnect becomes TCP_CONNECT; the first
// TypeExitUDP becomes UDP_OPEN and the following ones UDP_PKT.
func (s *clientStream) WriteFrame(t net_types.MessageType, data []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	switch {
	case t == net_types.TypeExitConnect && s.mode == modeNew:
		host, port, err := splitTarget(string(data))
		if err != nil {
			return err
		}
		s.mode = modeTCP
		return s.ws.WriteFrame(TypeTCPConnect, 0, TCPConnect{Host: host, Port: port}.Append(nil))
	case t == net_types.TypeExitUDP && s.mode == modeNew:
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		s.req = binary.BigEndian.Uint64(b[:]) | 1
		s.mode = modeUDP
		return s.ws.WriteFrame(TypeUDPOpen, 0, UDPOpen{Req: s.req}.Append(nil))
	case t == net_types.TypeExitUDP && s.mode == modeUDP:
		addr, payload, err := parseCoreDatagram(data)
		if err != nil {
			return err
		}
		pkt := UDPPkt{Addr: addr, Data: payload}.Append(nil)
		if err := s.writePadding(len(pkt)); err != nil {
			return err
		}
		return s.ws.WriteFrame(TypeUDPPkt, 0, pkt)
	}
	return fmt.Errorf("exit: unexpected core frame %#x in tunnel mode %d", uint64(t), s.mode)
}

// writePadding writes the PADDING frame that rounds a record with an n-byte payload up (H1). The caller
// holds wmu.
func (s *clientStream) writePadding(n int) error {
	pad, _ := s.pad.plan(n)
	if pad <= 0 {
		return nil
	}
	return s.ws.WriteFrame(frame.TypePadding, frame.FlagIgnorable, make([]byte, pad))
}

// Write sends p as DATA frames of at most 16384 bytes, padding the first records (H1).
func (s *clientStream) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), frame.MaxMuxData)
		if err := s.writePadding(n); err != nil {
			return written, err
		}
		if err := s.ws.WriteFrame(TypeData, 0, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// ReadFrame returns the exit's answer as the v1 core frame: RESULT for a TCP tunnel, UDP_OK for a
// UDP tunnel (code, then the datagram session id when there is one), then UDP_PKT as datagrams.
func (s *clientStream) ReadFrame() (net_types.MessageType, []byte, error) {
	for {
		f, err := s.ws.ReadFrame()
		if err != nil {
			return 0, nil, err
		}
		s.wmu.Lock()
		mode, req := s.mode, s.req
		s.wmu.Unlock()
		switch {
		case f.Type == TypeResult && mode == modeTCP:
			r, err := ParseResult(f.Payload)
			if err != nil {
				return 0, nil, err
			}
			return net_types.TypeExitConnectResult, []byte{resultByte(r.Code)}, nil
		case f.Type == TypeUDPOK && mode == modeUDP:
			ok, err := ParseUDPOK(f.Payload)
			if err != nil {
				return 0, nil, err
			}
			if ok.Req != req {
				return 0, nil, errors.New("exit: UDP_OK for another request")
			}
			out := []byte{resultByte(ok.Code)}
			if ok.Code == ResultOK && ok.Session != 0 {
				out = binary.BigEndian.AppendUint32(out, ok.Session)
			}
			return net_types.TypeExitConnectResult, out, nil
		case f.Type == TypeUDPPkt && mode == modeUDP:
			u, err := ParseUDPPkt(f.Payload)
			if err != nil {
				return 0, nil, err
			}
			return net_types.TypeExitUDP, coreDatagram(u.Addr, u.Data), nil
		case f.Type == frame.TypeGrease:
			continue
		}
		return 0, nil, fmt.Errorf("exit: frame %#x from the exit in tunnel mode %d", f.Type, mode)
	}
}

// resultByte maps an exit/2 RESULT code to the byte the v1 core understands. 0..4 are the same
// by design; every other code reads as "could not reach" there.
func resultByte(code uint32) byte {
	if code <= 4 {
		return byte(code)
	}
	return byte(ResultUnreachable)
}

// Read returns the payload of the next DATA frame.
func (s *clientStream) Read(p []byte) (int, error) {
	if len(s.buf) > 0 {
		n := copy(p, s.buf)
		s.buf = s.buf[n:]
		return n, nil
	}
	for {
		f, err := s.ws.ReadFrame()
		if err != nil {
			return 0, err
		}
		if f.Type != TypeData {
			return 0, fmt.Errorf("exit: frame %#x on a TCP tunnel", f.Type)
		}
		if len(f.Payload) == 0 {
			continue
		}
		n := copy(p, f.Payload)
		if n < len(f.Payload) {
			s.buf = append([]byte(nil), f.Payload[n:]...)
		}
		return n, nil
	}
}

// CloseWrite sends FIN: the TCP FIN of the tunnel.
func (s *clientStream) CloseWrite() error { return s.ws.CloseWrite() }

// Close ends the tunnel.
func (s *clientStream) Close() error { return s.ws.Close() }
