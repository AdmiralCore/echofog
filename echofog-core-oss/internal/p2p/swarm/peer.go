package swarm

import (
	"context"
	"errors"
	"log"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/handshake"

	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/network/router"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"

	"google.golang.org/protobuf/proto"
)

type Peer struct {
	transport *network.PeerWrapper
	router    *router.PeerTransportRouter
	id        types.PeerID // this now ed25519 peer pub key
	pubKey    types.PeerPublicKey
	addr      string
	isOut     bool

	sendChan chan *internal_pb.Envelope
	ctx      context.Context
	cancel   context.CancelFunc

	mu sync.RWMutex

	txBytes *atomic.Uint64

	caps atomic.Uint32 // what the peer announced it offers (handshake.Capability)

	ip             netip.Addr   // where an inbound peer connected from (limits and bans count against it)
	connectedAt    time.Time    // a long-lived peer is kept when the node is full
	release        func()       // gives back its place in the per-address limits; nil for outbound
	pendingStreams atomic.Int32 // streams it opened whose first frame has not come yet

	// primaryReality reports whether the primary transport is a QUIC-REALITY connection. For a
	// camouflage exit it must be true; the swarm never downgrades such a peer's primary to noise.
	primaryReality bool

	// extraLinks are additional data-carrying connections to the same peer, besides transport.
	// They exist only for an exit peer whose session pool runs several parallel connections:
	// tunnel streams spread across transport and these, so a single stalled or censor-killed
	// connection does not stall the VPN. New tunnel streams go to the newest link
	// (openTunnelStream) and older links are closed once idle (retireOldLinks), so each connection
	// carries about one session's worth of bytes, as flow shaping wants. A mesh peer never has any;
	// its control plane keeps using transport alone. Guarded by mu.
	extraLinks []exitLink

	// opening counts, per link, the tunnel streams chosen to open on it whose stream is not yet
	// registered on the link (ActiveStreams counts a stream only once it is open). A link with one
	// is busy: retiring it would cut the stream off before it starts. Guarded by mu.
	opening map[*network.PeerWrapper]int

	// primarySince is when the current primary connection came up: connectedAt, or the moment a
	// pool link was promoted, keeping that link's own start. Guarded by mu.
	primarySince time.Time

	dispatcher *dispatcher.Dispatcher
}

func NewPeer(ctx context.Context, peerPubKey types.PeerPublicKey, dispatcher *dispatcher.Dispatcher, addr string, isOut bool) *Peer {
	ctx, cancel := context.WithCancel(ctx)

	return &Peer{
		id:          types.PeerPubKeyToID(peerPubKey),
		pubKey:      peerPubKey,
		addr:        addr,
		isOut:       isOut,
		sendChan:    make(chan *internal_pb.Envelope, 100),
		ctx:         ctx,
		cancel:      cancel,
		dispatcher:  dispatcher,
		connectedAt: time.Now(),
	}
}

func (p *Peer) Close() {
	p.cancel()

	p.mu.Lock()
	r := p.router
	tw := p.transport
	extra := p.extraLinks
	p.extraLinks = nil
	p.mu.Unlock()

	if r != nil {
		_ = r.Close()
	}
	if tw != nil {
		_ = tw.Close()
	}
	for _, l := range extra {
		_ = l.pw.Close()
	}
}

// Alive reports whether the peer's connection is still open.
func (p *Peer) Alive() bool {
	p.mu.RLock()
	tw := p.transport
	p.mu.RUnlock()
	return p.ctx.Err() == nil && tw != nil && tw.GetConn().Context().Err() == nil
}

func (p *Peer) SetTransport(transport *network.PeerWrapper) {
	p.SetTransportWithProtocol(transport, router.ProtoDirectQUIC)
}

func (p *Peer) SetTransportWithProtocol(transport *network.PeerWrapper, proto router.TransportProtocol) {
	p.mu.Lock()
	p.transport = transport
	link := NewPeerLink(transport, proto)
	if p.router == nil {
		p.router = router.NewPeerTransportRouter(p.ctx, p.id, link)
	} else {
		_ = p.router.InitiateMigration(link, "transport rebind")
	}
	p.mu.Unlock()
}

func (p *Peer) SetRouter(r *router.PeerTransportRouter) {
	p.mu.Lock()
	p.router = r
	p.mu.Unlock()
}

func (p *Peer) Router() *router.PeerTransportRouter {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.router
}

func (p *Peer) ActiveProtocol() router.TransportProtocol {
	p.mu.RLock()
	r := p.router
	p.mu.RUnlock()

	if r != nil {
		return r.ActiveProtocol()
	}
	return router.ProtoDirectQUIC
}

func (p *Peer) MigrateTransport(targetLink router.PeerTransportLink, reason string) error {
	p.mu.RLock()
	r := p.router
	p.mu.RUnlock()

	if r == nil {
		return errors.New("peer: router is nil, cannot migrate")
	}
	return r.InitiateMigration(targetLink, reason)
}

func (p *Peer) ID() types.PeerID {
	return p.id
}

// RemoteAddr is the network address of the current connection.
func (p *Peer) RemoteAddr() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.transport == nil {
		return ""
	}
	return p.transport.RemoteAddr().String()
}

func (p *Peer) PubKey() types.PeerPublicKey {
	return p.pubKey
}

// Addr is where this peer accepts connections; empty when unknown (an inbound TCP peer that
// did not announce a port). The transport's remote address is RemoteAddr.
func (p *Peer) Addr() string {
	return p.addr
}

func (p *Peer) Send(msgType net_types.MessageType, msgData *internal_pb.Envelope) error {
	data, err := proto.Marshal(msgData)
	if err != nil {
		log.Println(err)
		return err
	}

	return p.SendRaw(msgType, data)
}

func (p *Peer) SendRaw(msgType net_types.MessageType, data []byte) error {
	if p.txBytes != nil {
		p.txBytes.Add(uint64(len(data)))
	}
	p.mu.RLock()
	r := p.router
	tw := p.transport
	p.mu.RUnlock()

	if r != nil {
		return r.Send(msgType, data)
	}
	if tw == nil {
		return errors.New("peer transport is nil")
	}
	return tw.SendGossipMessage(msgType, data)
}

// SendRawUnreliable sends a loss-tolerant frame, preferring QUIC datagrams.
func (p *Peer) SendRawUnreliable(msgType net_types.MessageType, data []byte) error {
	if p.txBytes != nil {
		p.txBytes.Add(uint64(len(data)))
	}
	p.mu.RLock()
	r := p.router
	tw := p.transport
	p.mu.RUnlock()

	if r != nil {
		return r.SendUnreliable(msgType, data)
	}
	if tw == nil {
		return errors.New("peer transport is nil")
	}
	return tw.SendDatagramFrame(msgType, data)
}

// SetCaps records what the peer announced in the handshake.
func (p *Peer) SetCaps(c handshake.Capability) { p.caps.Store(uint32(c)) }

// Caps is what the peer announced it offers. Peers of versions before the announcement was used
// always said "relay".
func (p *Peer) Caps() handshake.Capability { return handshake.Capability(p.caps.Load()) }

// Relays tells whether the peer relays traffic for others. A peer that announced nothing (added
// without a handshake) counts as one, as every node did before the announcement was used.
// ConnectedAt is when the connection to the peer came up.
func (p *Peer) ConnectedAt() time.Time { return p.connectedAt }

func (p *Peer) Relays() bool {
	c := p.Caps()
	return c == handshake.CapNone || c.Has(handshake.CapRelay)
}

// Signals tells whether the peer passes on hole punching messages (a relay does too).
func (p *Peer) Signals() bool { return p.Relays() || p.Caps().Has(handshake.CapSignal) }

// ActiveStreams returns the number of active streams on the underlying transport.
func (p *Peer) ActiveStreams() int {
	p.mu.RLock()
	tw := p.transport
	p.mu.RUnlock()
	if tw != nil {
		return tw.ActiveStreams()
	}
	return 0
}

// primaryIsCamouflaged reports whether the primary connection is safe to carry tunnel traffic to a
// camouflage exit: a QUIC-REALITY link, or any TCP/WS link (the TCP fallback is browser-TLS
// mimicry). Only a plain QUIC noise link is not.
func (p *Peer) primaryIsCamouflaged() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.transport == nil {
		return false
	}
	return !quicNoise(p.transport.GetConn().Protocol(), p.primaryReality)
}

// hasCamouflagedLink reports whether any live connection to this peer — the primary or a pool
// link — is camouflaged (QUIC-REALITY or TCP/WS mimicry), i.e. can carry a camouflage exit's tunnel.
func (p *Peer) hasCamouflagedLink() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.transport != nil && p.transport.GetConn().Context().Err() == nil &&
		!quicNoise(p.transport.GetConn().Protocol(), p.primaryReality) {
		return true
	}
	for _, l := range p.extraLinks {
		if l.pw.GetConn().Context().Err() == nil && !quicNoise(l.pw.GetConn().Protocol(), l.reality) {
			return true
		}
	}
	return false
}

// exitLink is one parallel connection of an exit peer's session pool. reality records whether it
// is a QUIC-REALITY connection, so a camouflage exit's tunnel streams are kept off any noise link.
type exitLink struct {
	pw      *network.PeerWrapper
	reality bool
	since   time.Time // when the link was added
}

// addExitLink records an additional parallel connection to this peer (an exit's session pool).
func (p *Peer) addExitLink(pw *network.PeerWrapper, reality bool) {
	p.mu.Lock()
	p.extraLinks = append(p.extraLinks, exitLink{pw: pw, reality: reality, since: time.Now()})
	p.mu.Unlock()
}

// promoteExitLink makes a live parallel pool link the peer's primary connection once the primary
// has died, preferring a camouflaged link over a QUIC noise one, and returns it; nil when the
// primary is still alive or no live link is left. proto gives the router protocol of a connection.
func (p *Peer) promoteExitLink(proto func(network.MuxConn) router.TransportProtocol) *network.PeerWrapper {
	p.mu.Lock()
	if p.transport != nil && p.transport.GetConn().Context().Err() == nil {
		p.mu.Unlock()
		return nil
	}
	best := -1
	for i, l := range p.extraLinks {
		if l.pw.GetConn().Context().Err() != nil {
			continue
		}
		if best < 0 || (quicNoise(p.extraLinks[best].pw.GetConn().Protocol(), p.extraLinks[best].reality) &&
			!quicNoise(l.pw.GetConn().Protocol(), l.reality)) {
			best = i
		}
	}
	if best < 0 {
		p.mu.Unlock()
		return nil
	}
	next := p.extraLinks[best]
	p.extraLinks = append(p.extraLinks[:best], p.extraLinks[best+1:]...)
	dead := p.transport
	p.primaryReality = next.reality
	p.primarySince = next.since
	p.mu.Unlock()

	p.SetTransportWithProtocol(next.pw, proto(next.pw.GetConn()))
	if dead != nil {
		_ = dead.Close()
	}
	return next.pw
}

// closeExitLinks closes and forgets every extra pool link, keeping the primary connection.
func (p *Peer) closeExitLinks() {
	p.mu.Lock()
	extra := p.extraLinks
	p.extraLinks = nil
	p.mu.Unlock()
	for _, l := range extra {
		_ = l.pw.Close()
	}
}

// removeExitLink drops pw from the extra-link set (e.g. once its connection died). It does not
// close pw; the caller owns that.
func (p *Peer) removeExitLink(pw *network.PeerWrapper) {
	p.mu.Lock()
	for i, l := range p.extraLinks {
		if l.pw == pw {
			p.extraLinks = append(p.extraLinks[:i], p.extraLinks[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
}

// LinkCount is how many live connections this peer currently holds: the primary transport plus
// every still-open extra link.
func (p *Peer) LinkCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	if p.transport != nil && p.transport.GetConn().Context().Err() == nil {
		n++
	}
	for _, l := range p.extraLinks {
		if l.pw.GetConn().Context().Err() == nil {
			n++
		}
	}
	return n
}

// linkBusyLocked reports whether pw carries a stream or one is being opened on it. p.mu held.
func (p *Peer) linkBusyLocked(pw *network.PeerWrapper) bool {
	return pw.ActiveStreams() > 0 || p.opening[pw] > 0
}

// newestLinkLocked returns the most recently added live link — the last live extra link, else
// the primary — skipping QUIC noise links when camouflageExit is set; nil if there is none. p.mu
// held. (A promoted link was the oldest extra, so the primary is never newer than an extra.)
func (p *Peer) newestLinkLocked(camouflageExit bool) *network.PeerWrapper {
	for i := len(p.extraLinks) - 1; i >= 0; i-- {
		l := p.extraLinks[i]
		if l.pw.GetConn().Context().Err() != nil {
			continue
		}
		if camouflageExit && quicNoise(l.pw.GetConn().Protocol(), l.reality) {
			continue
		}
		return l.pw
	}
	if p.transport != nil && p.transport.GetConn().Context().Err() == nil &&
		(!camouflageExit || !quicNoise(p.transport.GetConn().Protocol(), p.primaryReality)) {
		return p.transport
	}
	return nil
}

// newestLinkAge is how long the link a new tunnel stream would ride (newestLinkLocked) has been up;
// ok is false when there is none.
func (p *Peer) newestLinkAge(camouflageExit bool) (age time.Duration, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	newest := p.newestLinkLocked(camouflageExit)
	if newest == nil {
		return 0, false
	}
	if newest != p.transport {
		for _, l := range p.extraLinks {
			if l.pw == newest {
				return time.Since(l.since), true
			}
		}
	}
	since := p.primarySince
	if since.IsZero() {
		since = p.connectedAt
	}
	return time.Since(since), true
}

// retireOldLinks closes every link but the newest that carries no stream and has none being
// opened: its share of the rotation is done. This includes the primary — the watcher then makes a
// remaining link the primary — so no connection to an exit lives on carrying gigabytes. A busy old
// link is left alone and retired once its transfer ends. Returns how many links it closed.
func (p *Peer) retireOldLinks(camouflageExit bool) int {
	p.mu.Lock()
	newest := p.newestLinkLocked(camouflageExit)
	if newest == nil {
		p.mu.Unlock()
		return 0
	}
	var retire []*network.PeerWrapper
	var keep []exitLink
	for _, l := range p.extraLinks {
		if l.pw != newest && !p.linkBusyLocked(l.pw) {
			retire = append(retire, l.pw)
			continue
		}
		keep = append(keep, l)
	}
	p.extraLinks = keep
	if p.transport != nil && p.transport != newest && p.transport.GetConn().Context().Err() == nil &&
		!p.linkBusyLocked(p.transport) {
		retire = append(retire, p.transport)
	}
	p.mu.Unlock()
	for _, pw := range retire {
		_ = pw.Close()
	}
	return len(retire)
}

// openTunnelStream opens one bidirectional stream to this peer on its newest live link, so the
// streams of a fresh session ride the fresh connection its rotation dialed and the older ones drain.
// The link is reserved until the stream is registered on it, so retireOldLinks never closes a link
// a stream is being opened on. When camouflageExit is set, only camouflaged links are considered —
// QUIC-REALITY or any TCP/WS (mimicry) link — so a tunnel stream never rides a plain QUIC noise
// connection a censor would flag; it returns an error if the peer has no live camouflaged link. For
// a mesh peer (no extra links, camouflageExit false) it is the primary transport.
func (p *Peer) openTunnelStream(ctx context.Context, camouflageExit bool) (*network.Stream, error) {
	p.mu.Lock()
	best := p.newestLinkLocked(camouflageExit)
	if best == nil {
		p.mu.Unlock()
		if camouflageExit {
			return nil, errors.New("camouflage exit has no live camouflaged (REALITY or TCP) link to open a stream on")
		}
		return nil, errors.New("peer has no live link to open a stream on")
	}
	if p.opening == nil {
		p.opening = make(map[*network.PeerWrapper]int)
	}
	p.opening[best]++
	p.mu.Unlock()

	st, err := best.OpenBidirectionalStream(ctx)

	p.mu.Lock()
	if p.opening[best]--; p.opening[best] <= 0 {
		delete(p.opening, best)
	}
	p.mu.Unlock()
	return st, err
}

// DrainAndClose waits up to timeout for active streams to finish, then closes the peer.
func (p *Peer) DrainAndClose(timeout time.Duration) {
	go func() {
		deadline := time.After(timeout)
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			if p.ActiveStreams() == 0 {
				p.Close()
				return
			}
			select {
			case <-deadline:
				p.Close()
				return
			case <-ticker.C:
			}
		}
	}()
}
