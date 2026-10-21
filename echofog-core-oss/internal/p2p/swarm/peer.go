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
	p.mu.Unlock()

	if r != nil {
		_ = r.Close()
	}
	if tw != nil {
		_ = tw.Close()
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
func (p *Peer) Relays() bool {
	c := p.Caps()
	return c == handshake.CapNone || c.Has(handshake.CapRelay)
}

// Signals tells whether the peer passes on hole punching messages (a relay does too).
func (p *Peer) Signals() bool { return p.Relays() || p.Caps().Has(handshake.CapSignal) }
