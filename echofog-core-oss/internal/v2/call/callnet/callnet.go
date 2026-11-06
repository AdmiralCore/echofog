// Package callnet plugs the call layer (internal/services/call: trees, layers, media) into v2
// sessions (T9.3). It implements the call layer's transport port: media units are CALL_AUDIO and
// CALL_VIDEO datagrams of the session with the participant, on the path kind call_route allows
// (package route); signals go to a SignalPort.
//
// A relay between two participants carries only SEALED datagrams of their end-to-end session
// (wire.md 8.4): it cannot read, forge or reorder media, and the one nonce counter of the session
// is shared by every path, so two relay paths and a direct one never reuse a nonce.
package callnet

import (
	"errors"
	"sync"

	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// MaxDatagram is the largest media payload sent as one datagram (the 1232-byte floor of every
// path, wire.md 6.5). A larger frame is refused with transport.ErrDatagramTooLarge.
const MaxDatagram = 1232

var (
	// ErrNoLink: no live session with the participant.
	ErrNoLink = errors.New("callnet: no session with the peer")
	// ErrMediaNeedsDatagram: media is never sent on a reliable stream.
	ErrMediaNeedsDatagram = errors.New("callnet: media travels only as datagrams")
)

// Links finds the live session with a participant.
type Links interface {
	Link(device [32]byte) (route.Link, bool)
}

// SignalPort carries call signals; the node implements it over call/2 (T9.1).
type SignalPort interface {
	SendSignal(to [32]byte, lossy bool, payload []byte) error
}

// Config wires a Transport.
type Config struct {
	Links   Links
	Signals SignalPort
	// Route is read on every send, so call_route can change between calls. Nil means Auto.
	Route func() route.Route
	// Admit tells whether media from a device is wanted (a member of the room being joined).
	// Media from anyone else is dropped before the call layer sees it. Nil admits everyone.
	Admit func(device [32]byte) bool
}

// Stats counts what Deliver dropped and what could not be sent.
type Stats struct {
	DroppedRoute, DroppedAdmit, DroppedUnknown, SendNoPath uint64
}

// Transport is the call layer's transport over v2 sessions.
type Transport struct {
	cfg      Config
	mu       sync.RWMutex
	handlers map[transport.Kind]transport.Handler
	stats    Stats
}

var (
	_ transport.Transport  = (*Transport)(nil)
	_ transport.RelayAware = (*Transport)(nil)
	_ transport.DirectLink = (*Transport)(nil)
)

// New returns a transport.
func New(cfg Config) *Transport {
	return &Transport{cfg: cfg, handlers: map[transport.Kind]transport.Handler{}}
}

// Inbound is one datagram received from a participant's session.
type Inbound struct {
	Device  [32]byte
	Type    uint64
	Payload []byte
	Path    mux.PathKind
}

// SendRawFrame sends a reliable frame: signals only.
func (t *Transport) SendRawFrame(peer types.PeerID, kind transport.Kind, payload []byte) error {
	return errors.New("callnet: not implemented")
}

// SendUnreliableFrame sends a best-effort frame: media datagrams and lossy signals.
func (t *Transport) SendUnreliableFrame(peer types.PeerID, kind transport.Kind, payload []byte) error {
	return errors.New("callnet: not implemented")
}

// RegisterFastFrameHandler sets the handler of one kind of inbound frame.
func (t *Transport) RegisterFastFrameHandler(kind transport.Kind, h transport.Handler) {}

// Deliver hands a received datagram to the call layer.
func (t *Transport) Deliver(in Inbound) {}

// Relayed reports whether the participant is reached only through a relay.
func (t *Transport) Relayed(peer types.PeerID) bool { return false }

// DirectLink reports whether the call layer should not ask for a connection to the peer.
func (t *Transport) DirectLink(peer types.PeerID) bool { return false }

// Stats returns the counters.
func (t *Transport) Stats() Stats { return Stats{} }
