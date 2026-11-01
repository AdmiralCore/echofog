package net_types

type MessageType uint32

const (
	TypeUnknown MessageType = iota
	TypeHandshake
	TypeReady
	TypeGossip
	TypePing
	TypePong
	TypeChatMessage
	TypeDatagram
	TypeGetPeerRequest
	TypeGetPeerResponse
	TypeBlockRequest
	TypeBlockResponse
	TypeSessionInitRequest
	TypeDAGSessionInitRequest
	TypeDAGSessionInitResponse
	TypeSessionInitResponse
	TypeStreamInitRequest
	TypeChunkRequest
	TypeChunkResponse
	TypeStreamCancel
	TypePuzzleChallengeRequest
	TypePuzzleChallengeResponse
	TypeSessionData
	TypeLocalPublicKeyBundleRequest
	TypeLocalPublicKeyBundleResponse
	TypeRelayMessageRequest
	TypeWantList
	TypeBlockData
	TypeFindNode
	TypeFindNodeRes
	TypeStore
	TypeFindValue
	TypeFindValueRes
	TypeRelayRejected
	TypeMailboxFull
	TypeCircuitOpenRequest
	TypeCircuitOpenResponse
	TypeCircuitData
	TypeCircuitClose
	TypeTransportMigrateRequest
	TypeTransportMigrateAck
	TypeCallSignaling
	TypeCallMediaAudio
	TypeCallMediaVideo
	TypeBandwidthProbe
	TypeExitConnect
	TypeExitConnectResult
	// TypeExitUDP opens a UDP relay on an exit; afterwards every frame on the stream is one
	// datagram: [2-byte address length]["host:port"][payload].
	TypeExitUDP
	// TypeExitAccount asks an exit for the caller's payment account: price, where and how to pay, and the
	// remaining balance. The reply is one TypeExitAccount frame holding JSON.
	TypeExitAccount
	// TypeObservedAddr asks a peer which address it sees our connection coming from (our public address
	// as the outside world sees it, with the NAT mapping applied). The reply is one frame with "ip:port".
	TypeObservedAddr
	// TypeHolePunch carries the coordination messages of a direct-connection attempt between two peers
	// behind NATs. It travels over a relay, because the peers cannot reach each other yet.
	TypeHolePunch
	// TypeExitVoucher offers an exit a signed voucher from the client's TON vault, paying for the
	// next chunk of traffic. The first frame holds the JSON offer, the reply one JSON frame.
	TypeExitVoucher
	// TypeExitDirectory asks a peer for the exit adverts it knows. The reply is one frame holding a
	// JSON array of adverts exactly as their exits signed them, so the asker verifies each itself.
	// It is how a client finds exits it has no connection to and whose IDs it has never seen.
	TypeExitDirectory
	// TypeExitUDPToExit and TypeExitUDPFromExit carry the datagrams of an exit UDP session as
	// unreliable QUIC datagrams, outside the session's stream: [4-byte session ID][2-byte packet
	// ID][fragment index][fragment count][2-byte address length]["host:port"][payload].
	TypeExitUDPToExit
	TypeExitUDPFromExit
	// TypeFileXfer carries one frame of a file transfer (see internal/services/filexfer): sealed
	// end to end, so it travels the same way directly and over a relay circuit.
	TypeFileXfer
	// TypeCallSignalingLossy is a call signal sent as a datagram where late is as bad as lost: a
	// NACK. On a stream it waits behind every packet the path lost before it, and a resend asked
	// for a second late finds nothing left to resend. The same signal goes by stream too, for nodes
	// that do not know this type; the receiver takes whichever comes first.
	TypeCallSignalingLossy
)

// IsUnreliableType reports whether frames of this type may arrive as unreliable QUIC datagrams.
func IsUnreliableType(t MessageType) bool {
	return t == TypeCallMediaAudio || t == TypeCallMediaVideo || t == TypeExitUDPToExit || t == TypeExitUDPFromExit ||
		t == TypeCallSignalingLossy
}

// IsDatagramFrame reports whether a frame of this type is accepted from a datagram: the
// unreliable types, and a relay circuit's data, whose media frames travel there between peers that
// announced handshake.CapCircuitMedia (the frames of a circuit that are not media still go by stream).
func IsDatagramFrame(t MessageType) bool {
	return IsUnreliableType(t) || t == TypeCircuitData
}
