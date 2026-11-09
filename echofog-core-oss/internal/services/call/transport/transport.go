// Package transport is the port the call service uses to send and receive its frames.
//
// The call layer does not know the network stack: it names the kinds of call frames it has
// (signals and media datagrams), and the node supplies an adapter for its transport. That is what
// keeps internal/services/call free of the v1 network, so a v2 node can plug the call service in.
package transport

import (
	"errors"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Kind is what a call frame carries.
type Kind uint8

const (
	// Signal is call signaling sent reliably: joins, tree manifests, heartbeats.
	Signal Kind = iota
	// SignalLossy is call signaling where late is as bad as lost (a NACK). The node may send it
	// as a datagram, so it does not queue behind the stream.
	SignalLossy
	// MediaAudio is voice and a stream's sound.
	MediaAudio
	// MediaVideo is video down a stream's distribution tree.
	MediaVideo
	// MediaAuth is the batch authenticator of media units (CALL_AUTH on call/2). A transport that
	// has no such frame never sends or receives it.
	MediaAuth
)

// Handler receives one inbound call frame; from is the transport peer it came from.
type Handler func(payload []byte, from types.PeerID) error

// FrameSender sends call frames to a peer: SendRawFrame reliably, SendUnreliableFrame best-effort.
// Both name the peer by identity, never by a transport-specific peer object.
type FrameSender interface {
	SendRawFrame(peer types.PeerID, kind Kind, payload []byte) error
	SendUnreliableFrame(peer types.PeerID, kind Kind, payload []byte) error
}

// Transport is what the call service needs from the node: the send side, and a place to register
// a handler for each kind of inbound frame.
type Transport interface {
	FrameSender
	RegisterFastFrameHandler(kind Kind, handler Handler)
}

// RelayAware is implemented by a transport that can tell which peers it reaches only through a
// relay circuit: the call layer then stretches the watchdogs for that extra hop.
type RelayAware interface {
	Relayed(peer types.PeerID) bool
}

// DirectLink is implemented by a transport that knows whether it already has a direct link to a
// peer, so the call layer does not ask for a connection it has.
type DirectLink interface {
	DirectLink(peer types.PeerID) bool
}

// DatagramFloor is implemented by a transport whose datagrams carry fewer bytes than the call
// layer's v1 budget (media.DatagramBudget, 1242). The v2 sessions work on the 1232-byte floor of
// every path (wire.md 2.4, callnet.MaxDatagram); media seals fragments to fit it, or the transport
// refuses them with ErrDatagramTooLarge and the media is lost.
type DatagramFloor interface {
	DatagramFloor() int
}

// ErrDatagramTooLarge reports that an unreliable frame did not fit into one datagram; the sender
// may put it on the reliable path instead.
var ErrDatagramTooLarge = errors.New("call frame does not fit into a datagram")
