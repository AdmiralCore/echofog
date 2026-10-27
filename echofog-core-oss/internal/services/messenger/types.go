package messenger

import (
	"encoding/json"
	"hash/fnv"
)

// EncryptionTier specifies the security and cryptographic guarantee for a message
type EncryptionTier int

const (
	// TierX3DH provides forward secrecy and post-compromise security via Signal X3DH + Double Ratchet.
	TierX3DH EncryptionTier = iota
	// TierChaCha20 provides fast, stateless authenticated symmetric encryption via XChaCha20-Poly1305.
	TierChaCha20
	// TierPlain provides signed-only unencrypted transmission (Ed25519 identity envelope verified, plaintext payload).
	TierPlain
)

func (t EncryptionTier) String() string {
	switch t {
	case TierX3DH:
		return "x3dh"
	case TierChaCha20:
		return "chacha20"
	case TierPlain:
		return "plain"
	default:
		return "unknown"
	}
}

// MessageMetadata holds structured application and transport metadata
type MessageMetadata struct {
	AppID        string         `json:"app_id,omitempty"`
	NumericAppID uint32         `json:"numeric_app_id,omitempty"`
	EncTier      string         `json:"enc_tier,omitempty"`
	FileName     string         `json:"file_name,omitempty"`
	FileSize     int64          `json:"file_size,omitempty"`
	FileKeyHex   string         `json:"file_key_hex,omitempty"`
	MimeType     string         `json:"mime_type,omitempty"`
	Custom       map[string]any `json:"custom,omitempty"`
}

// SendOptions configures advanced message sending parameters
type SendOptions struct {
	Tier        EncryptionTier
	AppID       string
	SharedKey   []byte
	ReplyToID   string
	Type        PersonalMessageTypeProto
	ContentHash string
	Metadata    *MessageMetadata
	// Silent messages are protocol traffic (group events): sent and queued like any message but
	// never stored in the chat history of this node.
	Silent bool

	// ID replaces the random message id: a conversation-log event id, so receipts and delivery
	// states name the event.
	ID string
	// ConvEvent / ConvSync carry a conversation-log event or sync message (package convlog).
	ConvEvent []byte
	ConvSync  []byte
	// NoTrack sends once and does not wait for a receipt (sync traffic, reactions: the log's sync
	// makes up for a loss).
	NoTrack bool
	// DirectOnly hands the message only to the peer itself, never to a relay mailbox (sync
	// traffic, which is useless to an absent peer).
	DirectOnly bool

	onQueued func() // called when the message could not go out at once
	noTrack  bool   // receipts: do not wait for a receipt of the receipt
}

// EncodeMetadata serializes MessageMetadata into a JSON string
func EncodeMetadata(meta *MessageMetadata) string {
	if meta == nil {
		return ""
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return ""
	}
	return string(data)
}

// DecodeMetadata parses a JSON string into MessageMetadata
func DecodeMetadata(raw string) MessageMetadata {
	var meta MessageMetadata
	if raw == "" {
		return meta
	}
	_ = json.Unmarshal([]byte(raw), &meta)
	return meta
}

// ResolveNumericAppID deterministically maps a string AppID into a uint32 for storage keys
func ResolveNumericAppID(appID string) uint32 {
	switch appID {
	case "", "echofog.chat", "chat":
		return AppChatID
	case "echofog.ai", "ai":
		return AppAiID
	default:
		h := fnv.New32a()
		h.Write([]byte(appID))
		val := h.Sum32()
		if val < 100 {
			val += 100
		}
		return val
	}
}
