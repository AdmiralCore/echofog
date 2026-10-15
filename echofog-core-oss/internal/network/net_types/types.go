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
)

// IsUnreliableType reports whether frames of this type may arrive as unreliable QUIC datagrams.
func IsUnreliableType(t MessageType) bool {
	return t == TypeCallMediaAudio || t == TypeCallMediaVideo
}
