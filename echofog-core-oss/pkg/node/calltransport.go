package node

import (
	"errors"
	"fmt"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// callWireType maps the call transport port onto the swarm's frame types. This file is the only
// place that knows both vocabularies; the call service itself sees only transport.Kind.
func callWireType(kind transport.Kind) (net_types.MessageType, bool) {
	switch kind {
	case transport.Signal:
		return net_types.TypeCallSignaling, true
	case transport.SignalLossy:
		return net_types.TypeCallSignalingLossy, true
	case transport.MediaAudio:
		return net_types.TypeCallMediaAudio, true
	case transport.MediaVideo:
		return net_types.TypeCallMediaVideo, true
	}
	return net_types.TypeUnknown, false
}

// callKindOf is the reverse of callWireType for frames that arrive over a relay circuit.
func callKindOf(t net_types.MessageType) (transport.Kind, bool) {
	switch t {
	case net_types.TypeCallSignaling:
		return transport.Signal, true
	case net_types.TypeCallSignalingLossy:
		return transport.SignalLossy, true
	case net_types.TypeCallMediaAudio:
		return transport.MediaAudio, true
	case net_types.TypeCallMediaVideo:
		return transport.MediaVideo, true
	}
	return 0, false
}

// wireLink is how the v1 transport takes a frame: the swarm's own vocabulary, kept out of the
// call service.
type wireLink interface {
	SendRawFrame(peer types.PeerID, msgType net_types.MessageType, payload []byte) error
	SendUnreliableFrame(peer types.PeerID, msgType net_types.MessageType, payload []byte) error
}

type swarmCallLink interface {
	wireLink
	Relayed(peer types.PeerID) bool
}

// swarmCallTransport is the v1 adapter of the call transport port. Outbound frames go over the
// direct swarm link; when the node has relay circuits, a peer without one is reached over a
// circuit. Inbound handlers are registered on the swarm.
type swarmCallTransport struct {
	swarm *swarm.Swarm
	// link adds relay circuits for peers without a direct link; nil when the node has no circuits.
	link swarmCallLink
}

func newSwarmCallTransport(n *Node) *swarmCallTransport {
	t := &swarmCallTransport{swarm: n.Swarm}
	if n.RelayClient != nil {
		t.link = newCallLink(n)
	}
	return t
}

func (t *swarmCallTransport) SendRawFrame(peer types.PeerID, kind transport.Kind, payload []byte) error {
	msgType, ok := callWireType(kind)
	if !ok {
		return fmt.Errorf("call: unknown frame kind %d", kind)
	}
	if t.link != nil {
		return t.link.SendRawFrame(peer, msgType, payload)
	}
	return t.swarm.SendRawFrame(peer, msgType, payload)
}

func (t *swarmCallTransport) SendUnreliableFrame(peer types.PeerID, kind transport.Kind, payload []byte) error {
	msgType, ok := callWireType(kind)
	if !ok {
		return fmt.Errorf("call: unknown frame kind %d", kind)
	}
	var err error
	if t.link != nil {
		err = t.link.SendUnreliableFrame(peer, msgType, payload)
	} else {
		err = t.swarm.SendUnreliableFrame(peer, msgType, payload)
	}
	if errors.Is(err, network.ErrDatagramTooLarge) {
		// the call layer does not know the network package: give it its own sentinel
		return fmt.Errorf("%w: %v", transport.ErrDatagramTooLarge, err)
	}
	return err
}

func (t *swarmCallTransport) RegisterFastFrameHandler(kind transport.Kind, handler transport.Handler) {
	msgType, ok := callWireType(kind)
	if !ok {
		return
	}
	t.swarm.RegisterFastFrameHandler(msgType, func(payload []byte, from types.PeerID) error {
		return handler(payload, from)
	})
}

// DirectLink tells the call layer that a peer is already connected directly, so it does not ask
// the connector to dial it again.
func (t *swarmCallTransport) DirectLink(peer types.PeerID) bool {
	return t.swarm.GetPeer(peer) != nil
}

// Relayed tells the call layer that a peer is reached only through a relay circuit, so its
// watchdogs allow for the extra hop.
func (t *swarmCallTransport) Relayed(peer types.PeerID) bool {
	return t.link != nil && t.link.Relayed(peer)
}
