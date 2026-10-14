package router

import (
	"encoding/json"
	"fmt"

	"github.com/echofog/echofog-core/internal/network/net_types"
)

// TransportProtocol defines the underlying L4 transport flavor.
type TransportProtocol uint8

const (
	ProtoDirectQUIC TransportProtocol = 1 // Raw 1Gbps+ QUIC on open networks
	ProtoObfsQUIC   TransportProtocol = 2 // Salamander per-packet obfuscation against DPI
	ProtoTCPTLS     TransportProtocol = 3 // REALITY-style TLS 1.3 browser mimicry on port 443
)

func (p TransportProtocol) String() string {
	switch p {
	case ProtoDirectQUIC:
		return "DirectQUIC"
	case ProtoObfsQUIC:
		return "ObfsQUIC"
	case ProtoTCPTLS:
		return "TCPTLS_Mimicry"
	default:
		return fmt.Sprintf("Unknown(%d)", p)
	}
}

// LinkState defines the runtime operational health of a peer link.
type LinkState int32

const (
	StateActive    LinkState = 1 // Link is passing packets normally
	StateDegraded  LinkState = 2 // High packet loss / latency detected
	StateMigrating LinkState = 3 // Socket failover in progress; buffering egress
	StateClosed    LinkState = 4 // Connection terminated
)

func (s LinkState) String() string {
	switch s {
	case StateActive:
		return "Active"
	case StateDegraded:
		return "Degraded"
	case StateMigrating:
		return "Migrating"
	case StateClosed:
		return "Closed"
	default:
		return fmt.Sprintf("Unknown(%d)", s)
	}
}

// MigrationPayload encodes the wire protocol for peer-to-peer transport handovers.
type MigrationPayload struct {
	TargetProtocol TransportProtocol `json:"target_protocol"`
	SessionNonce   uint64            `json:"session_nonce"`
	Timestamp      int64             `json:"timestamp"`
	Reason         string            `json:"reason"`
}

func (m *MigrationPayload) Encode() ([]byte, error) {
	return json.Marshal(m)
}

func DecodeMigrationPayload(data []byte) (*MigrationPayload, error) {
	var p MigrationPayload
	err := json.Unmarshal(data, &p)
	return &p, err
}

// PeerTransportLink abstracts the physical connection to a specific peer.
type PeerTransportLink interface {
	Protocol() TransportProtocol
	Send(msgType net_types.MessageType, payload []byte) error
	Close() error
}

// UnreliableLink is implemented by links that can carry loss-tolerant frames without retransmission.
type UnreliableLink interface {
	SendUnreliable(msgType net_types.MessageType, payload []byte) error
}

// BufferedPacket holds an outbound packet buffered during transport migration.
type BufferedPacket struct {
	MsgType net_types.MessageType
	Payload []byte
}
