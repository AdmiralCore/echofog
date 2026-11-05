package exit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// maxExitDatagram is the largest EXIT_UDP payload: a MUX_DATAGRAM carries 1452 bytes including
// its one-byte type (registries.md §1).
const maxExitDatagram = mux.MaxDatagram - 1

// errTooLarge: the reply does not fit in eight EXIT_UDP fragments; it goes as UDP_PKT instead.
var errTooLarge = errors.New("exit: datagram too large for EXIT_UDP")

// coreResultCode maps a v1 exit result byte to a v2 RESULT code. The v1 codes are the v2 codes
// 0..4 by design; anything unknown is unreachable, never an "open" answer (D19).
func coreResultCode(b byte) uint32 {
	switch b {
	case 0, 1, 2, 3, 4:
		return uint32(b)
	}
	return ResultUnreachable
}

// tcpAdapter is the core.FramedStream of one TCP tunnel: the core writes the result frame and
// then treats the stream as a byte pipe, while the adapter translates to DATA frames and pads the
// first records (H1).
type tcpAdapter struct {
	st  *mux.Stream
	pad *padder
	clk carrier.Clock

	mu         sync.Mutex
	resultSent bool
	timer      carrier.Timer

	buf []byte
}

func newTCPAdapter(st *mux.Stream, prof *shape.Profile, sleep func(time.Duration), clk carrier.Clock) *tcpAdapter {
	return &tcpAdapter{st: st, pad: newPadder(prof, sleep, nil), clk: clk}
}

// arm answers the client with unreachable if the core has not produced a result in time: the
// spec bounds TCP_CONNECT to RESULT at 10 s.
func (a *tcpAdapter) arm(d time.Duration) {
	a.mu.Lock()
	if a.resultSent {
		a.mu.Unlock()
		return
	}
	a.timer = a.clk.AfterFunc(d, func() {
		_ = a.writeResult(ResultUnreachable)
		_ = a.st.Close()
	})
	a.mu.Unlock()
}

func (a *tcpAdapter) writeResult(code uint32) error {
	a.mu.Lock()
	if a.resultSent {
		a.mu.Unlock()
		return nil
	}
	a.resultSent = true
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.mu.Unlock()
	return a.st.WriteFrame(TypeResult, 0, Result{Code: code}.Append(nil))
}

// WriteFrame is the core's result frame. The core writes nothing else with frames on a TCP
// tunnel; data goes through Write.
func (a *tcpAdapter) WriteFrame(t net_types.MessageType, data []byte) error {
	if t != net_types.TypeExitConnectResult {
		return fmt.Errorf("exit: unexpected core frame %#x on a TCP tunnel", uint64(t))
	}
	if len(data) < 1 {
		return errors.New("exit: empty core result")
	}
	return a.writeResult(coreResultCode(data[0]))
}

// Read returns the payload of the next DATA frame.
func (a *tcpAdapter) Read(p []byte) (int, error) {
	if len(a.buf) > 0 {
		n := copy(p, a.buf)
		a.buf = a.buf[n:]
		return n, nil
	}
	for {
		f, err := a.st.ReadFrame()
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
			a.buf = append([]byte(nil), f.Payload[n:]...)
		}
		return n, nil
	}
}

// Write splits p into DATA frames of at most 16384 bytes (services.md §7) and pads the first
// records of the direction (H1).
func (a *tcpAdapter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), frame.MaxMuxData)
		chunk := p[:n]
		if a.pad != nil {
			if pad, _ := a.pad.plan(len(chunk)); pad > 0 {
				if err := a.st.WriteFrame(frame.TypePadding, frame.FlagIgnorable, make([]byte, pad)); err != nil {
					return written, err
				}
			}
		}
		if err := a.st.WriteFrame(TypeData, 0, chunk); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (a *tcpAdapter) CloseWrite() error { return a.st.CloseWrite() }
func (a *tcpAdapter) Close() error      { return a.st.Close() }

// ReadFrame is part of core.FramedStream; the core never reads a frame on a TCP tunnel (the
// target came in TCP_CONNECT).
func (a *tcpAdapter) ReadFrame() (net_types.MessageType, []byte, error) {
	return 0, nil, errors.New("exit: no framed request on a TCP tunnel")
}

// udpAdapter is the core.FramedStream of one UDP tunnel. Datagrams from the client are reassembled
// by the server and delivered to ReadFrame; replies are fragmented into EXIT_UDP datagrams (or go
// back as UDP_PKT when the session cannot carry datagrams).
type udpAdapter struct {
	st         *mux.Stream
	peer       Peer
	req        uint64
	id         uint32
	maxTargets int
	pad        *padder
	frags      *reassembler
	clk        carrier.Clock

	stCh  chan frame.Frame
	inbox chan udpReply
	done  chan struct{}

	closeOnce sync.Once
	mu        sync.Mutex
	opened    bool
	session   uint32
	timer     carrier.Timer
	pkt       uint16
	targets   map[string]struct{}
}

type udpReply struct {
	addr string
	data []byte
}

func newUDPAdapter(st *mux.Stream, p Peer, req uint64, id uint32, cfg ServerConfig, clk carrier.Clock) *udpAdapter {
	a := &udpAdapter{
		st: st, peer: p, req: req, id: id, maxTargets: cfg.Limits.UDPTargets,
		pad: newPadder(cfg.Shape, cfg.Sleep, nil), frags: newReassembler(clk.Now), clk: clk,
		stCh: make(chan frame.Frame, 8), inbox: make(chan udpReply, 64),
		done: make(chan struct{}), targets: map[string]struct{}{},
	}
	go a.pump()
	return a
}

func (a *udpAdapter) pump() {
	defer close(a.stCh)
	for {
		f, err := a.st.ReadFrame()
		if err != nil {
			return
		}
		select {
		case a.stCh <- f:
		case <-a.done:
			return
		}
	}
}

func (a *udpAdapter) arm(d time.Duration) {
	a.mu.Lock()
	if a.opened {
		a.mu.Unlock()
		return
	}
	a.timer = a.clk.AfterFunc(d, func() {
		_ = a.writeOK(ResultUnreachable)
		_ = a.st.Close()
	})
	a.mu.Unlock()
}

func (a *udpAdapter) writeOK(code uint32) error {
	a.mu.Lock()
	if a.opened {
		a.mu.Unlock()
		return nil
	}
	a.opened = true
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	if code == ResultOK && a.peer.SendDatagram != nil {
		a.session = a.id
	}
	session := a.session
	a.mu.Unlock()
	return a.st.WriteFrame(TypeUDPOK, 0, UDPOK{Req: a.req, Session: session, Code: code}.Append(nil))
}

func (a *udpAdapter) WriteFrame(t net_types.MessageType, data []byte) error {
	switch t {
	case net_types.TypeExitConnectResult:
		if len(data) < 1 {
			return errors.New("exit: empty core result")
		}
		return a.writeOK(coreResultCode(data[0]))
	case net_types.TypeExitUDP:
		addr, payload, err := parseCoreDatagram(data)
		if err != nil {
			return err
		}
		return a.send(addr, payload)
	}
	return fmt.Errorf("exit: unexpected core frame %#x on a UDP tunnel", uint64(t))
}

func (a *udpAdapter) ReadFrame() (net_types.MessageType, []byte, error) {
	for {
		select {
		case f, ok := <-a.stCh:
			if !ok {
				return 0, nil, io.EOF
			}
			if f.Type != TypeUDPPkt {
				return 0, nil, fmt.Errorf("exit: frame %#x on a UDP tunnel", f.Type)
			}
			u, err := ParseUDPPkt(f.Payload)
			if err != nil {
				return 0, nil, err
			}
			if !a.admitTarget(u.Addr) {
				continue
			}
			return net_types.TypeExitUDP, coreDatagram(u.Addr, u.Data), nil
		case r := <-a.inbox:
			return net_types.TypeExitUDP, coreDatagram(r.addr, r.data), nil
		case <-a.done:
			return 0, nil, io.EOF
		}
	}
}

// deliver hands a reassembled client datagram to the core. A slow core loses, as a full socket
// buffer would.
func (a *udpAdapter) deliver(addr string, data []byte) {
	if !a.admitTarget(addr) {
		return
	}
	select {
	case a.inbox <- udpReply{addr: addr, data: append([]byte(nil), data...)}:
	default:
	}
}

// admitTarget enforces the 256-targets-per-UDP-session limit of §7.
func (a *udpAdapter) admitTarget(addr string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.targets[addr]; ok {
		return true
	}
	if len(a.targets) >= a.maxTargets {
		return false
	}
	a.targets[addr] = struct{}{}
	return true
}

func (a *udpAdapter) send(addr string, data []byte) error {
	a.mu.Lock()
	datagrams := a.session != 0 && a.peer.SendDatagram != nil
	a.mu.Unlock()
	if datagrams {
		if err := a.sendDatagrams(addr, data); err == nil {
			return nil
		}
	}
	pkt := UDPPkt{Addr: addr, Data: data}
	if a.pad != nil {
		if pad, _ := a.pad.plan(frame.FrameLen(TypeUDPPkt, len(pkt.Append(nil)))); pad > 0 {
			if err := a.st.WriteFrame(frame.TypePadding, frame.FlagIgnorable, make([]byte, pad)); err != nil {
				return err
			}
		}
	}
	return a.st.WriteFrame(TypeUDPPkt, 0, pkt.Append(nil))
}

// sendDatagrams splits one reply into at most eight EXIT_UDP datagrams (services.md §7).
func (a *udpAdapter) sendDatagrams(addr string, data []byte) error {
	if len(addr) > 254 {
		// addr_len counts the whole address part, so 254 is the longest address it can carry.
		return errTooLarge
	}
	a.mu.Lock()
	a.pkt++
	packet := a.pkt
	a.mu.Unlock()

	overhead := frame.VarintLen(uint64(a.id)) + 2 + 1 + 1 + 1 + len(addr)
	chunk := maxExitDatagram - overhead
	if chunk < 1 {
		return errTooLarge
	}
	count := (len(data) + chunk - 1) / chunk
	if count == 0 {
		count = 1
	}
	if count > MaxFragments {
		return errTooLarge
	}
	for i := 0; i < count; i++ {
		start := min(i*chunk, len(data))
		end := min((i+1)*chunk, len(data))
		f := Fragment{Session: uint64(a.id), Packet: packet, Index: uint8(i), Count: uint8(count)}
		if i == 0 {
			f.Addr = addr
		}
		f.Data = data[start:end]
		if err := a.peer.SendDatagram(frame.DgramExitUDP, f.Append(nil)); err != nil {
			return err
		}
	}
	return nil
}

func (a *udpAdapter) Read(p []byte) (int, error) {
	return 0, errors.New("exit: read on a framed UDP tunnel")
}

func (a *udpAdapter) Write(p []byte) (int, error) {
	return 0, errors.New("exit: write on a framed UDP tunnel")
}

func (a *udpAdapter) Close() error {
	a.closeOnce.Do(func() {
		close(a.done)
		a.mu.Lock()
		if a.timer != nil {
			a.timer.Stop()
			a.timer = nil
		}
		a.mu.Unlock()
	})
	return a.st.Close()
}

// coreDatagram is the v1 core's UDP frame: addr_len(u16) ‖ addr ‖ payload.
func coreDatagram(addr string, data []byte) []byte {
	out := make([]byte, 2+len(addr)+len(data))
	binary.BigEndian.PutUint16(out, uint16(len(addr)))
	copy(out[2:], addr)
	copy(out[2+len(addr):], data)
	return out
}

func parseCoreDatagram(b []byte) (string, []byte, error) {
	if len(b) < 2 {
		return "", nil, errors.New("exit: short core datagram")
	}
	n := int(binary.BigEndian.Uint16(b))
	if n == 0 || n > maxAddrLen || len(b) < 2+n {
		return "", nil, errors.New("exit: malformed core datagram")
	}
	return string(b[2 : 2+n]), b[2+n:], nil
}
