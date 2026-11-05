package session

import (
	"crypto/ecdh"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// Role is the side of the handshake this node played.
type Role uint8

const (
	// RoleInitiator is the side that dialed and sent HS_HELLO.
	RoleInitiator Role = iota + 1
	// RoleResponder is the side that accepted the carrier.
	RoleResponder
)

func (r Role) String() string {
	switch r {
	case RoleInitiator:
		return "initiator"
	case RoleResponder:
		return "responder"
	}
	return "unknown"
}

// LocalIdentity is one local identity as the session layer needs it: the device keys, the card
// for message 3, and the offers. The caller owns it; Dial and Accept never destroy or modify it.
type LocalIdentity struct {
	// Device supplies the Noise static key hs and the device ID (required).
	Device *identity.Device
	// Card is the local card put into HsFinish (P3): the peer-scope card of Device, or the
	// minimal card of a throwaway identity for XX to a bare address. Required by Dial.
	Card []byte
	// OwnCards are the cards this identity answers with in message 2 (Accept only).
	OwnCards handshake.OwnCards
	// AccessSecret is the 16-byte invitation or personal pass secret (XK to a hidden node).
	AccessSecret []byte
	// Suites, Features and Services are what this identity offers or accepts. A nil Suites means
	// the handshake default (XK before XX).
	Suites   []uint16
	Features []uint32
	Services []handshake.ServiceOffer
	// Throwaway marks a one-use identity. It is required for XX to a bare address (handshake
	// §4.2) and confirms that Device, Card and hs are never reused.
	Throwaway bool
}

// Established is one live session after the handshake. Session is ready for mux streams and
// datagrams; every other field is the verified result of the handshake.
type Established struct {
	Session *mux.Session
	// Peer is the verified peer card (handshake §7). It is never nil on a completed session.
	Peer *handshake.CardInfo
	Role Role
	// Suite is the L1 suite: XKhfs or XXhfs.
	Suite uint16
	SID   [16]byte
	// Features and Services are the negotiated values (GREASE removed, responder selection).
	Features []uint32
	Services []handshake.ServiceChoice
	// Remote is the peer's carrier address when the carrier exposes one (Dial: the dialed
	// address); it is the zero value for carriers without an address (in-memory tests).
	Remote netip.AddrPort
	// Local is the identity this side used.
	Local LocalIdentity
	// Unverified is set on XX to a bare address without an expected device ID: every card and
	// address learned in the session is only a candidate (handshake §4.2).
	Unverified bool
	// Observed is the initiator's address as seen by the responder (feature observed-addr).
	Observed      []byte
	EarlyStatus   uint32
	EarlyResponse []byte
	// Access names the pass that admitted a hidden-node session (responder side only). The runtime
	// redeems an invitation or renews a personal pass with it (T3.6).
	Access *handshake.AccessInfo
}

// Errors a caller distinguishes locally. On the wire the handshake follows handshake.md §8:
// REJECT frames only where the specification allows them, otherwise a silent close.
var (
	// ErrSilent means the carrier was closed without a message: a malformed handshake, a hidden
	// node's uniform failure, a refused PATH_BIND, or a local give-up.
	ErrSilent = errors.New("session: peer closed silently")
	// ErrPathBound means stream 0 began with a valid PATH_BIND and the carrier became a path of
	// an existing session; nothing new was established.
	ErrPathBound = errors.New("session: carrier bound as a path of an existing session")
	// ErrConfig is a local wiring error (no device, no card, no carrier).
	ErrConfig = errors.New("session: invalid configuration")
)

// ErrRejected is a protocol rejection: a REJECT received from the peer or the local decision
// that produced one (for example an invalid card after message 3). Reason is a
// registries.md §10.1 value.
type ErrRejected struct {
	Reason     uint32
	Min        uint32
	Max        uint32
	Detail     string
	RetryAfter uint32
	Cause      error
}

func (e *ErrRejected) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("session: rejected (reason %d): %s", e.Reason, e.Detail)
	}
	return fmt.Sprintf("session: rejected (reason %d)", e.Reason)
}

// Unwrap exposes the local cause, if any.
func (e *ErrRejected) Unwrap() error { return e.Cause }

func rejection(r handshake.Reject) *ErrRejected {
	return &ErrRejected{
		Reason: r.Reason, Min: r.Min, Max: r.Max, Detail: r.Detail, RetryAfter: r.RetryAfter,
	}
}

// mapHandshakeError maps a handshake state-machine error to a distinguishable local error.
func mapHandshakeError(err error) error {
	if err == nil {
		return nil
	}
	var rej *handshake.Reject
	if errors.As(err, &rej) {
		return rejection(*rej)
	}
	switch {
	case errors.Is(err, handshake.ErrPeerCard), errors.Is(err, handshake.ErrKeyBinding):
		return &ErrRejected{Reason: handshake.ReasonCardInvalid, Cause: err}
	case errors.Is(err, handshake.ErrVersion):
		return &ErrRejected{Reason: handshake.ReasonVersion, Cause: err}
	case errors.Is(err, handshake.ErrClockSkew):
		return &ErrRejected{Reason: handshake.ReasonClockSkew, Cause: err}
	}
	// A selection violation aborts without a message (handshake §7 and §11); it is a protocol
	// error, not a REJECT.
	return wrapSilent(err)
}

// wrapSilent keeps the cause while making the local error match ErrSilent.
func wrapSilent(err error) error {
	if err == nil {
		return ErrSilent
	}
	return fmt.Errorf("%w: %v", ErrSilent, err)
}

// sessionKeys adapts a completed handshake to mux.Keys (mux/keys.go).
type sessionKeys struct{ hs *handshake.Session }

func (k sessionKeys) SID() [16]byte {
	var sid [16]byte
	copy(sid[:], k.hs.SID)
	return sid
}

func (k sessionKeys) Initiator() bool { return k.hs.Initiator }

func (k sessionKeys) SessionSecret() []byte { return append([]byte(nil), k.hs.SessionSecret...) }

func (k sessionKeys) TranscriptHash() []byte { return append([]byte(nil), k.hs.H...) }

func (k sessionKeys) TransportKeys() (i2r, r2i []byte) {
	return k.hs.KI2R.Key(), k.hs.KR2I.Key()
}

// selectedPolicy lets the session layer open the service pairs negotiated in the handshake. The
// service package replaces it in node.Runtime with per-type limiters; here every selected pair
// is usable and carries only session-level frames (PADDING) until then.
type selectedPolicy map[[2]uint64]struct{}

func (p selectedPolicy) Limiter(service, version uint64) (frame.Limiter, bool) {
	if _, ok := p[[2]uint64{service, version}]; !ok {
		return nil, false
	}
	return func(uint64) (int, bool) { return 0, false }, true
}

func policyFor(choices []handshake.ServiceChoice) mux.StreamPolicy {
	if len(choices) == 0 {
		return nil
	}
	p := make(selectedPolicy, len(choices))
	for _, c := range choices {
		p[[2]uint64{uint64(c.ID), uint64(c.Version)}] = struct{}{}
	}
	return p
}

// muxFeatures converts the negotiated handshake features to the mux feature set.
func muxFeatures(features []uint32) []uint16 {
	out := make([]uint16, 0, len(features))
	for _, f := range features {
		if f <= 0xffff {
			out = append(out, uint16(f))
		}
	}
	return out
}

// systemClock is the production clock; a caller may inject another carrier.Clock (AD13).
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, f func()) carrier.Timer { return time.AfterFunc(d, f) }

func clockOr(c carrier.Clock) carrier.Clock {
	if c != nil {
		return c
	}
	return systemClock{}
}

// staticKey extracts the X25519 private key of the device's hs key pair.
func staticKey(dev *identity.Device) (*ecdh.PrivateKey, error) {
	if dev == nil {
		return nil, fmt.Errorf("%w: no device", ErrConfig)
	}
	var key *ecdh.PrivateKey
	err := dev.Handshake().WithPrivate(func(b []byte) error {
		var err error
		key, err = ecdh.X25519().NewPrivateKey(b)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("%w: hs key: %v", ErrConfig, err)
	}
	if key == nil {
		return nil, fmt.Errorf("%w: no hs key", ErrConfig)
	}
	return key, nil
}

// verifyRemote verifies the peer card against the injected clock and maps every card failure to
// ErrRejected{card-invalid}.
func verifyRemote(card *objects.Card, now time.Time, v handshake.CardVerifier) (*handshake.CardInfo, error) {
	if card == nil {
		return nil, fmt.Errorf("%w: no card", ErrConfig)
	}
	if card.Env == nil || len(card.Env.Raw) == 0 {
		return nil, fmt.Errorf("%w: card has no envelope", ErrConfig)
	}
	if v == nil {
		v = handshake.DefaultCardVerifier{}
	}
	info, err := v.VerifyCard(card.Env.Raw, now)
	if err != nil {
		return nil, &ErrRejected{Reason: handshake.ReasonCardInvalid, Cause: err}
	}
	return info, nil
}

// finish hands the handshake result to the mux. mux.NewSession makes the single sealing switch
// (wire.md §8.5) when the carrier reports Sealed.
func finish(c mux.Carrier, g *mux.Guard, ctrl mux.RawStream, hs *handshake.Session, role Role, kind mux.PathKind, local LocalIdentity, remote netip.AddrPort, ctl *Control, timers mux.Timers) (*Established, error) {
	// The handshake read deadline belongs to the handshake, not to the session: its control
	// stream must wait for peer traffic indefinitely (the mux applies its own timers).
	_ = ctrl.SetReadDeadline(time.Time{})
	_ = ctrl.SetWriteDeadline(time.Time{})
	cfg := mux.Config{
		Keys:     sessionKeys{hs: hs},
		Features: muxFeatures(hs.Features),
		Policy:   policyFor(hs.Services),
		Timers:   timers,
	}
	var peerID [32]byte
	if hs.Peer != nil {
		peerID = hs.Peer.DeviceID
	}
	if ctl != nil {
		cfg.OnControl = func(_ uint64, f frame.Frame) { ctl.handle(peerID, f) }
	}
	ms, err := mux.NewSession(cfg, g, ctrl, kind)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("%w: mux session: %w", ErrConfig, err)
	}
	var sid [16]byte
	copy(sid[:], hs.SID)
	return &Established{
		Session:       ms,
		Peer:          hs.Peer,
		Role:          role,
		Suite:         hs.Suite,
		SID:           sid,
		Features:      append([]uint32(nil), hs.Features...),
		Services:      append([]handshake.ServiceChoice(nil), hs.Services...),
		Remote:        remote,
		Local:         local,
		Unverified:    hs.Unverified,
		Observed:      append([]byte(nil), hs.Observed...),
		EarlyStatus:   hs.EarlyStatus,
		EarlyResponse: append([]byte(nil), hs.EarlyResponse...),
		Access:        hs.Access,
	}, nil
}

// writeFrame writes one session-level frame under the message timeout.
func writeFrame(w mux.RawStream, clk carrier.Clock, typ uint64, payload []byte) error {
	if err := w.SetWriteDeadline(clk.Now().Add(handshake.MessageTimeout)); err != nil {
		return err
	}
	_, err := w.Write(frame.AppendFrame(nil, frame.Frame{Type: typ, Payload: payload}))
	return err
}

// readFrame reads one frame with a deadline.
func readFrame(rd *frame.Reader, w mux.RawStream, deadline time.Time) (frame.Frame, error) {
	if err := w.SetReadDeadline(deadline); err != nil {
		return frame.Frame{}, err
	}
	return rd.ReadFrame()
}

// messageDeadline is min(whole handshake, per-message wait + now).
func messageDeadline(clk carrier.Clock, whole time.Time) time.Time {
	d := clk.Now().Add(handshake.MessageTimeout)
	if d.After(whole) {
		return whole
	}
	return d
}

// closeAfterFlush lets a final REJECT leave before the carrier closes and does not block longer
// than one second when the peer does not close first.
func closeAfterFlush(c mux.Carrier, clk carrier.Clock) {
	if f, ok := c.(interface{ CloseAfterFlush(time.Duration) }); ok {
		f.CloseAfterFlush(time.Second)
		return
	}
	ch := make(chan struct{})
	tm := clk.AfterFunc(time.Second, func() { close(ch) })
	select {
	case <-c.Done():
	case <-ch:
	}
	tm.Stop()
	_ = c.Close()
}

// waitUntil waits for t on the injected clock (a hidden node's uniform close).
func waitUntil(clk carrier.Clock, t time.Time) {
	d := t.Sub(clk.Now())
	if d <= 0 {
		return
	}
	ch := make(chan struct{})
	tm := clk.AfterFunc(d, func() { close(ch) })
	<-ch
	tm.Stop()
}

// remoteOf reads a carrier's peer address when the carrier exposes one.
func remoteOf(c mux.Carrier) netip.AddrPort {
	r, ok := c.(interface{ RemoteAddr() net.Addr })
	if !ok {
		return netip.AddrPort{}
	}
	ap, err := netip.ParseAddrPort(r.RemoteAddr().String())
	if err != nil {
		return netip.AddrPort{}
	}
	return ap
}

// remoteOfCard converts a card address to a netip.AddrPort for Established.Remote.
func remoteOfCard(a *pb.Addr) netip.AddrPort {
	if a == nil || a.Host == "" || a.Port == 0 || a.Port > 65535 {
		return netip.AddrPort{}
	}
	ip, err := netip.ParseAddr(a.Host)
	if err != nil {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(a.Port))
}
