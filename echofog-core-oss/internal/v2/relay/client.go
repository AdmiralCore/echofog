package relay

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Timeouts of services.md §6.1.
const (
	ConnectTimeout  = 8 * time.Second
	RegisterTimeout = 5 * time.Second
)

// RefusedError is REFUSED from R. Reason Unavailable says nothing about why (§6.3).
type RefusedError struct{ Reason uint32 }

func (e *RefusedError) Error() string {
	if e.Reason == ReasonLimit {
		return "relay: refused: limit"
	}
	return "relay: refused: unavailable"
}

// ErrProtocol: R or B broke the relay/2 framing.
var ErrProtocol = errors.New("relay: protocol violation")

// Opener opens a relay/2 stream on the session with R (service.Open on that session).
type Opener func(ctx context.Context) (*mux.Stream, error)

// ConnectOptions are the optional parts of CONNECT.
type ConnectOptions struct {
	Mode       uint32 // default ModeStream
	TargetKind uint32
	WantAddr   bool
	// Timeout overrides ConnectTimeout (tests).
	Timeout time.Duration
}

func readFrameTimeout(st *mux.Stream, d time.Duration) (frame.Frame, error) {
	_ = st.SetReadDeadline(time.Now().Add(d))
	f, err := st.ReadFrame()
	_ = st.SetReadDeadline(time.Time{})
	return f, err
}

func randU64() uint64 {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])
}

func connect(ctx context.Context, open Opener, relayNode, target [32]byte, opt ConnectOptions) (*mux.Stream, uint64, [32]byte, netip.AddrPort, error) {
	if opt.Mode == 0 {
		opt.Mode = ModeStream
	}
	if opt.Timeout == 0 {
		opt.Timeout = ConnectTimeout
	}
	var cb [32]byte
	st, err := open(ctx)
	if err != nil {
		return nil, 0, cb, netip.AddrPort{}, err
	}
	st.SetLimiter(limiter())
	req := randU64()
	c := Connect{Req: req, Target: target[:], Mode: opt.Mode, TargetKind: opt.TargetKind, WantAddr: opt.WantAddr}
	if err := st.WriteFrame(TypeConnect, 0, c.Append(nil)); err != nil {
		return nil, 0, cb, netip.AddrPort{}, err
	}
	f, err := readFrameTimeout(st, opt.Timeout)
	if err != nil {
		st.Reset(frame.CodePathFailed)
		return nil, 0, cb, netip.AddrPort{}, fmt.Errorf("relay: waiting for CONNECTED: %w", err)
	}
	switch f.Type {
	case TypeRefused:
		r, err := ParseRefused(f.Payload)
		if err != nil || r.Req != req {
			st.Reset(frame.CodeProtocolViolation)
			return nil, 0, cb, netip.AddrPort{}, ErrProtocol
		}
		_ = st.Close()
		return nil, 0, cb, netip.AddrPort{}, &RefusedError{Reason: r.Reason}
	case TypeConnected:
		r, err := ParseConnected(f.Payload)
		if err != nil || r.Req != req || r.CID == 0 || r.CID > MaxCID || string(r.Target) != string(target[:]) {
			st.Reset(frame.CodeProtocolViolation)
			return nil, 0, cb, netip.AddrPort{}, ErrProtocol
		}
		var peer netip.AddrPort
		if opt.WantAddr {
			pa, err := readPeerAddr(st, r.CID, opt.Timeout)
			if err != nil {
				st.Reset(frame.CodeProtocolViolation)
				return nil, 0, cb, netip.AddrPort{}, err
			}
			peer = pa
		}
		return st, r.CID, [32]byte(handshake.CircuitBinding(relayNode[:], r.CID)), peer, nil
	}
	st.Reset(frame.CodeProtocolViolation)
	return nil, 0, cb, netip.AddrPort{}, ErrProtocol
}

// Dial asks R (relayNode is R's node ID, taken from R's own session or card, never from the
// network) for a stream or signal circuit to target. On success the returned Circuit is a
// carrier whose first use is the end-to-end handshake with the target.
func Dial(ctx context.Context, open Opener, relayNode, target [32]byte, opt ConnectOptions) (*Circuit, error) {
	if opt.Mode == ModeDatagram {
		return nil, errors.New("relay: use DialDatagram for datagram circuits")
	}
	st, cid, cb, peer, err := connect(ctx, open, relayNode, target, opt)
	if err != nil {
		return nil, err
	}
	c := NewCircuit(st.Detach(), cb, cid, true)
	c.peerAddr = peer
	return c, nil
}

// DialDatagram asks R for a datagram circuit. send is SendDatagram of the session with R; the
// circuit's datagrams are RELAY_DGRAM on that session, and Hub.HandleDatagram feeds the
// replies. The circuit stays valid while its anchor stream is open.
func DialDatagram(ctx context.Context, open Opener, hub *Hub, send func(typ uint64, p []byte) error,
	relayNode, target [32]byte, opt ConnectOptions) (*DatagramCircuit, error) {
	opt.Mode = ModeDatagram
	st, cid, _, _, err := connect(ctx, open, relayNode, target, opt)
	if err != nil {
		return nil, err
	}
	d, err := hub.newCircuit(relayNode, cid, send, st)
	if err != nil {
		// R offered an ID this node already holds for it: the new circuit is closed, the old one
		// keeps its route.
		st.Reset(frame.CodeProtocolViolation)
		return nil, err
	}
	return d, nil
}

// Inbound is what a node that received INCOMING holds: exactly one of Stream and Datagram is set
// when the circuit was accepted.
type Inbound struct {
	Incoming Incoming
	// PeerAddr is A's address as R observed it, set when both ends agreed to address exchange.
	PeerAddr netip.AddrPort
	Stream   *Circuit
	Datagram *DatagramCircuit
}

// Answer is the B side: it reads INCOMING from the stream R opened, lets ok decide, and answers
// ACCEPT or REFUSE. relayNode is the node ID of the session that carried the stream (R); it keys
// the circuit binding. INCOMING.source is only a hint: the end-to-end handshake authenticates A
// (K8). hub and send are used for datagram circuits only.
func Answer(st *mux.Stream, relayNode [32]byte, hub *Hub, send func(typ uint64, p []byte) error,
	ok func(Incoming) bool) (Inbound, error) {
	return AnswerAddr(st, relayNode, hub, send, ok, nil)
}

// AnswerAddr is Answer with the address exchange of services.md §6.4 step 1: when the INCOMING asked
// for addresses (want_addr) and wantAddr returns true, B answers ACCEPT{want_addr = true} and reads
// the PEER_ADDR that R then sends. A nil wantAddr never agrees.
func AnswerAddr(st *mux.Stream, relayNode [32]byte, hub *Hub, send func(typ uint64, p []byte) error,
	ok func(Incoming) bool, wantAddr func(Incoming) bool) (Inbound, error) {
	st.SetLimiter(limiter())
	f, err := readFrameTimeout(st, ConnectTimeout)
	if err != nil {
		st.Reset(frame.CodePathFailed)
		return Inbound{}, err
	}
	if f.Type != TypeIncoming {
		st.Reset(frame.CodeProtocolViolation)
		return Inbound{}, ErrProtocol
	}
	in, err := ParseIncoming(f.Payload)
	if err != nil || in.CID == 0 || in.CID > MaxCID || in.Mode < ModeStream || in.Mode > ModeSignal {
		st.Reset(frame.CodeProtocolViolation)
		return Inbound{}, ErrProtocol
	}
	res := Inbound{Incoming: in}
	if ok != nil && !ok(in) {
		_ = st.WriteFrame(TypeRefuse, 0, Refuse{CID: in.CID}.Append(nil))
		_ = st.Close()
		return res, &RefusedError{Reason: ReasonUnavailable}
	}
	var dg *DatagramCircuit
	if in.Mode == ModeDatagram {
		// Register before ACCEPT: an ID that is already in use at this node for R is refused,
		// not silently taking over the route of the circuit that holds it.
		var err error
		if dg, err = hub.newCircuit(relayNode, in.CID, send, st); err != nil {
			_ = st.WriteFrame(TypeRefuse, 0, Refuse{CID: in.CID}.Append(nil))
			_ = st.Close()
			return res, err
		}
	}
	agree := in.WantAddr && wantAddr != nil && wantAddr(in)
	if err := st.WriteFrame(TypeAccept, 0, Accept{CID: in.CID, WantAddr: agree}.Append(nil)); err != nil {
		if dg != nil {
			dg.Close()
		}
		return res, err
	}
	if agree {
		pa, err := readPeerAddr(st, in.CID, ConnectTimeout)
		if err != nil {
			if dg != nil {
				dg.Close()
			}
			st.Reset(frame.CodeProtocolViolation)
			return res, err
		}
		res.PeerAddr = pa
	}
	if dg != nil {
		res.Datagram = dg
		return res, nil
	}
	cb := [32]byte(handshake.CircuitBinding(relayNode[:], in.CID))
	res.Stream = NewCircuit(st.Detach(), cb, in.CID, false)
	res.Stream.peerAddr = res.PeerAddr
	return res, nil
}

// readPeerAddr reads the PEER_ADDR frame that follows CONNECTED (or ACCEPT) when the address
// exchange was asked for. An empty address means the other end did not agree or R cannot place it.
func readPeerAddr(st *mux.Stream, cid uint64, d time.Duration) (netip.AddrPort, error) {
	f, err := readFrameTimeout(st, d)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("relay: waiting for PEER_ADDR: %w", err)
	}
	if f.Type != TypePeerAddr {
		return netip.AddrPort{}, ErrProtocol
	}
	pa, err := ParsePeerAddr(f.Payload)
	if err != nil || pa.CID != cid {
		return netip.AddrPort{}, ErrProtocol
	}
	if len(pa.Addr) == 0 {
		return netip.AddrPort{}, nil
	}
	ap, err := frame.ParseObservedAddr(pa.Addr)
	if err != nil {
		return netip.AddrPort{}, ErrProtocol
	}
	return ap, nil
}
