package exit_gateway

import (
	"encoding/binary"
	"errors"
	"sync"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/network/router"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// The framing and the sender live in core (udp_fragment.go); this file is the swarm-specific
// datagram link: the demultiplexer for a swarm's unreliable frames and the adapter that turns a
// swarm stream into a core.DatagramStream.

// udpDemux routes a swarm's exit UDP datagrams of one direction to their sessions.
type udpDemux struct {
	mu sync.RWMutex
	m  map[udpDemuxKey]func([]byte)
}

type udpDemuxKey struct {
	peer types.PeerID // the authenticated sender: a peer only ever reaches its own sessions
	id   uint32
}

type udpDemuxSlot struct {
	sw *swarm.Swarm
	t  net_types.MessageType
}

var udpDemuxes sync.Map // udpDemuxSlot -> *udpDemux

// demuxFor returns the demux for frames of type t arriving at sw, registering it on first use.
func demuxFor(sw *swarm.Swarm, t net_types.MessageType) *udpDemux {
	slot := udpDemuxSlot{sw, t}
	if d, ok := udpDemuxes.Load(slot); ok {
		return d.(*udpDemux)
	}
	d, loaded := udpDemuxes.LoadOrStore(slot, &udpDemux{m: make(map[udpDemuxKey]func([]byte))})
	dm := d.(*udpDemux)
	if !loaded {
		sw.RegisterFastFrameHandler(t, dm.dispatch)
	}
	return dm
}

func (d *udpDemux) dispatch(payload []byte, peer types.PeerID) error {
	if len(payload) < core.UDPDgramHeader {
		return nil
	}
	d.mu.RLock()
	fn := d.m[udpDemuxKey{peer, binary.BigEndian.Uint32(payload)}]
	d.mu.RUnlock()
	if fn != nil {
		fn(payload[4:])
	}
	return nil // unknown sessions are dropped like stray UDP
}

func (d *udpDemux) add(peer types.PeerID, id uint32, fn func([]byte)) func() {
	k := udpDemuxKey{peer, id}
	d.mu.Lock()
	if d.m[k] != nil {
		d.mu.Unlock()
		return nil // taken
	}
	d.m[k] = fn
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		delete(d.m, k)
		d.mu.Unlock()
	}
}

// swarmUDPLink is a core.UDPLink over the swarm connection to one peer.
type swarmUDPLink struct {
	sw   *swarm.Swarm
	peer types.PeerID
	out  net_types.MessageType // what we send
	in   *udpDemux             // what the peer sends us
}

func (l *swarmUDPLink) Send(frame []byte) error {
	err := l.sw.SendUnreliableFrame(l.peer, l.out, frame)
	var tooLarge *network.DatagramTooLargeError
	if errors.As(err, &tooLarge) {
		return &core.DatagramTooLargeError{Size: tooLarge.Size, Max: tooLarge.Max}
	}
	return err
}

func (l *swarmUDPLink) Subscribe(id uint32, fn func([]byte)) func() {
	return l.in.add(l.peer, id, fn)
}

func (s swarmStream) UDPLink(toExit bool) core.UDPLink {
	if s.sw == nil {
		return nil
	}
	p := s.sw.GetPeer(s.RemoteID)
	if p == nil || p.ActiveProtocol() == router.ProtoTCPTLS {
		return nil // over the TCP fallback a datagram is no less reliable than the stream
	}
	out, in := net_types.TypeExitUDPFromExit, net_types.TypeExitUDPToExit
	if toExit {
		out, in = in, out
	}
	return &swarmUDPLink{sw: s.sw, peer: s.RemoteID, out: out, in: demuxFor(s.sw, in)}
}
