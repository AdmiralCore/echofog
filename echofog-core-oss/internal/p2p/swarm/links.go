package swarm

import (
	"errors"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/network/router"
)

// PeerLink adapts a network.PeerWrapper (QUIC, or TCP+smux) to the router.PeerTransportLink contract.
type PeerLink struct {
	wrapper *network.PeerWrapper
	proto   router.TransportProtocol
}

// NewPeerLink creates an L4 link wrapping an active peer connection.
func NewPeerLink(wrapper *network.PeerWrapper, proto router.TransportProtocol) *PeerLink {
	if proto == 0 {
		proto = router.ProtoDirectQUIC
	}
	return &PeerLink{
		wrapper: wrapper,
		proto:   proto,
	}
}

func (l *PeerLink) Protocol() router.TransportProtocol {
	return l.proto
}

func (l *PeerLink) Send(msgType net_types.MessageType, payload []byte) error {
	if l.wrapper == nil {
		return errors.New("quic link: peer wrapper is nil")
	}
	return l.wrapper.SendGossipMessage(msgType, payload)
}

func (l *PeerLink) SendUnreliable(msgType net_types.MessageType, payload []byte) error {
	if l.wrapper == nil {
		return errors.New("quic link: peer wrapper is nil")
	}
	return l.wrapper.SendDatagramFrame(msgType, payload)
}

func (l *PeerLink) Close() error {
	if l.wrapper == nil {
		return nil
	}
	return l.wrapper.Close()
}

func (l *PeerLink) Wrapper() *network.PeerWrapper {
	return l.wrapper
}
