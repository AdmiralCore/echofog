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
	"fmt"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/frame"
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
	// MaxPerSecond bounds the datagrams accepted from one device per second (a token bucket with
	// a burst of one second). Zero means DefaultMaxPerSecond. A peer cannot make this node spend
	// unbounded CPU on call frames.
	MaxPerSecond int
	// Now is the corrected clock (AD13). Nil means time.Now.
	Now func() time.Time
}

// DefaultMaxPerSecond: a 4K60 stream is about 3500 datagrams a second; the default leaves room for
// several streams relayed through this node.
const DefaultMaxPerSecond = 20000

// maxSources bounds the per-device buckets kept.
const maxSources = 1024

// Stats counts what Deliver dropped and what could not be sent.
type Stats struct {
	DroppedRoute, DroppedAdmit, DroppedUnknown, DroppedRate, SendNoPath uint64
	// SignalRefused counts signals the signal port refused, SignalReasons says why (a stable name
	// per reason), and LastSignalError keeps the last error text: a refused path report is not a
	// silent loss.
	SignalRefused   uint64
	SignalReasons   map[string]uint64
	LastSignalError string
}

// Transport is the call layer's transport over v2 sessions.
type Transport struct {
	cfg      Config
	mu       sync.RWMutex
	handlers map[transport.Kind]transport.Handler
	stats    Stats
	rate     map[[32]byte]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

var (
	_ transport.Transport  = (*Transport)(nil)
	_ transport.RelayAware = (*Transport)(nil)
	_ transport.DirectLink = (*Transport)(nil)
)

// New returns a transport.
func New(cfg Config) *Transport {
	return &Transport{cfg: cfg, handlers: map[transport.Kind]transport.Handler{}, rate: map[[32]byte]*bucket{}}
}

// Inbound is one datagram received from a participant's session.
type Inbound struct {
	Device  [32]byte
	Type    uint64
	Payload []byte
	Path    mux.PathKind
}

func (t *Transport) route() route.Route {
	if t.cfg.Route == nil {
		return route.Auto
	}
	return t.cfg.Route()
}

func (t *Transport) count(f func(*Stats)) {
	t.mu.Lock()
	f(&t.stats)
	t.mu.Unlock()
}

// SendRawFrame sends a reliable frame: signals only. Media never travels reliably.
func (t *Transport) SendRawFrame(peer types.PeerID, kind transport.Kind, payload []byte) error {
	switch kind {
	case transport.Signal, transport.SignalLossy:
		return t.signal(peer, false, payload)
	}
	return ErrMediaNeedsDatagram
}

// SendUnreliableFrame sends a best-effort frame: media datagrams and lossy signals.
func (t *Transport) SendUnreliableFrame(peer types.PeerID, kind transport.Kind, payload []byte) error {
	var typ uint64
	switch kind {
	case transport.MediaAudio:
		typ = frame.DgramCallAudio
	case transport.MediaVideo:
		typ = frame.DgramCallVideo
	case transport.MediaAuth:
		typ = frame.DgramCallAuth
	case transport.SignalLossy:
		return t.signal(peer, true, payload)
	default:
		return fmt.Errorf("callnet: kind %d is not sent unreliably", kind)
	}
	if len(payload) > MaxDatagram {
		return transport.ErrDatagramTooLarge
	}
	l, ok := t.cfg.Links.Link(peer)
	if !ok {
		return ErrNoLink
	}
	if _, err := route.Send(t.route(), l, typ, payload); err != nil {
		if errors.Is(err, route.ErrNoPath) {
			t.count(func(s *Stats) { s.SendNoPath++ })
		}
		return err
	}
	return nil
}

func (t *Transport) signal(peer types.PeerID, lossy bool, payload []byte) error {
	if t.cfg.Signals == nil {
		err := errors.New("callnet: no signal port")
		t.refuseSignal("no-port", err)
		return err
	}
	if err := t.cfg.Signals.SendSignal(peer, lossy, payload); err != nil {
		t.refuseSignal("port", err)
		return err
	}
	return nil
}

// refuseSignal keeps why a signal did not leave: a refused path report would otherwise be a
// silent loss, and the broadcaster would never learn the viewer's path is full.
func (t *Transport) refuseSignal(reason string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stats.SignalRefused++
	if t.stats.SignalReasons == nil {
		t.stats.SignalReasons = map[string]uint64{}
	}
	t.stats.SignalReasons[reason]++
	t.stats.LastSignalError = err.Error()
}

// RegisterFastFrameHandler sets the handler of one kind of inbound frame.
func (t *Transport) RegisterFastFrameHandler(kind transport.Kind, h transport.Handler) {
	t.mu.Lock()
	t.handlers[kind] = h
	t.mu.Unlock()
}

// admitRate takes one token from the device's bucket.
func (t *Transport) admitRate(device [32]byte) bool {
	limit := float64(t.cfg.MaxPerSecond)
	if limit <= 0 {
		limit = DefaultMaxPerSecond
	}
	now := time.Now
	if t.cfg.Now != nil {
		now = t.cfg.Now
	}
	at := now()
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.rate[device]
	if b == nil {
		if len(t.rate) >= maxSources {
			// Forget the device that has been quiet the longest.
			var oldest [32]byte
			var oldestAt time.Time
			first := true
			for d, x := range t.rate {
				if first || x.at.Before(oldestAt) {
					oldest, oldestAt, first = d, x.at, false
				}
			}
			delete(t.rate, oldest)
		}
		b = &bucket{tokens: limit, at: at}
		t.rate[device] = b
	}
	if el := at.Sub(b.at); el > 0 {
		b.tokens = min(limit, b.tokens+limit*el.Seconds())
		b.at = at
	}
	if b.tokens < 1 {
		t.stats.DroppedRate++
		return false
	}
	b.tokens--
	return true
}

// Deliver hands a received datagram to the call layer. Media is dropped before the call layer
// sees it when it arrived on a path kind the route forbids (call_route=relay accepts no media
// from a direct path) or comes from a device the room does not admit.
func (t *Transport) Deliver(in Inbound) {
	if !t.admitRate(in.Device) {
		return
	}
	var kind transport.Kind
	isMedia := false
	switch in.Type {
	case frame.DgramCallAudio:
		kind, isMedia = transport.MediaAudio, true
	case frame.DgramCallVideo:
		kind, isMedia = transport.MediaVideo, true
	case frame.DgramCallAuth:
		kind, isMedia = transport.MediaAuth, true
	case frame.DgramCallSignal:
		kind = transport.SignalLossy
	default:
		t.count(func(s *Stats) { s.DroppedUnknown++ })
		return
	}
	if isMedia {
		if k, err := route.Choose(t.route(), map[mux.PathKind]int{in.Path: 1}); err != nil || k != in.Path {
			t.count(func(s *Stats) { s.DroppedRoute++ })
			return
		}
		if t.cfg.Admit != nil && !t.cfg.Admit(in.Device) {
			t.count(func(s *Stats) { s.DroppedAdmit++ })
			return
		}
	}
	t.mu.RLock()
	h := t.handlers[kind]
	t.mu.RUnlock()
	if h != nil {
		_ = h(in.Payload, types.PeerID(in.Device))
	}
}

// DeliverSignal hands a verified call/2 signal, as a v1 envelope, to the call layer.
func (t *Transport) DeliverSignal(from [32]byte, v1 []byte) {
	if !t.admitRate(from) {
		return
	}
	t.mu.RLock()
	h := t.handlers[transport.Signal]
	if h == nil {
		h = t.handlers[transport.SignalLossy]
	}
	t.mu.RUnlock()
	if h != nil {
		_ = h(v1, types.PeerID(from))
	}
}

// Relayed reports whether the participant is reached through a relay, so the call layer stretches
// its watchdogs for the extra hop. Under call_route=relay media takes the relay path even when a
// direct session exists (the signalling stream is usually direct): such a peer is relayed too.
// A peer with no relay path is not: media cannot go to it at all. Under call_route=direct a relay
// path carries no media, so the peer is not "relayed"; under Auto it is only when that is the only
// path.
func (t *Transport) Relayed(peer types.PeerID) bool {
	l, ok := t.cfg.Links.Link(peer)
	if !ok {
		return false
	}
	kinds := l.PathKinds()
	switch t.route() {
	case route.Direct:
		return false
	case route.Relay:
		return kinds[mux.PathRelay] > 0
	}
	return route.OnlyRelayed(kinds)
}

// DirectLink reports whether the call layer should not ask for a connection to the peer. Under
// call_route=relay no direct path is wanted, so any live session counts: the call layer must not
// dial the participant directly.
func (t *Transport) DirectLink(peer types.PeerID) bool {
	l, ok := t.cfg.Links.Link(peer)
	if !ok {
		return false
	}
	kinds := l.PathKinds()
	if t.route() == route.Relay {
		return kinds[mux.PathDirect]+kinds[mux.PathRelay] > 0
	}
	return kinds[mux.PathDirect] > 0
}

// Stats returns the counters.
func (t *Transport) Stats() Stats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s := t.stats
	if len(s.SignalReasons) > 0 {
		s.SignalReasons = make(map[string]uint64, len(t.stats.SignalReasons))
		for k, v := range t.stats.SignalReasons {
			s.SignalReasons[k] = v
		}
	}
	return s
}
