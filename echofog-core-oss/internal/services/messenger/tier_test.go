package messenger

import (
	"bytes"
	"testing"

	"github.com/echofog/echofog-core/internal/crypto/encrypt_decrypt"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

func TestMessenger_TypesAndMetadata(t *testing.T) {
	meta := &MessageMetadata{
		AppID:        "community.chess",
		NumericAppID: ResolveNumericAppID("community.chess"),
		EncTier:      TierChaCha20.String(),
		FileName:     "move.json",
		Custom: map[string]any{
			"turn": 42,
		},
	}

	encoded := EncodeMetadata(meta)
	if encoded == "" {
		t.Fatalf("expected non-empty encoded metadata")
	}

	decoded := DecodeMetadata(encoded)
	if decoded.AppID != "community.chess" {
		t.Fatalf("expected app_id 'community.chess', got '%s'", decoded.AppID)
	}
	if decoded.EncTier != "chacha20" {
		t.Fatalf("expected enc_tier 'chacha20', got '%s'", decoded.EncTier)
	}
	if decoded.NumericAppID < 100 {
		t.Fatalf("expected custom app numeric ID >= 100, got %d", decoded.NumericAppID)
	}

	// Verify default Chat and AI numeric IDs
	if ResolveNumericAppID("echofog.chat") != AppChatID {
		t.Fatalf("expected AppChatID for echofog.chat")
	}
	if ResolveNumericAppID("echofog.ai") != AppAiID {
		t.Fatalf("expected AppAiID for echofog.ai")
	}
}

func TestMessenger_ChaCha20AndPlainPayloadEncoding(t *testing.T) {
	sharedKey := bytes.Repeat([]byte{0x42}, 32)

	innerMsg := &PersonalMessageProto{
		Id:        "00000000-0000-0000-0000-000000000001",
		SenderId:  "sender-peer-1",
		Type:      PersonalMessageTypeProto_MsgTypeText,
		Text:      "Checkmate!",
		Timestamp: 1,
	}
	innerMsg.Metadata = EncodeMetadata(&MessageMetadata{
		AppID:   "community.chess",
		EncTier: TierChaCha20.String(),
	})

	innerBytes, err := proto.Marshal(innerMsg)
	if err != nil {
		t.Fatalf("failed to marshal inner message: %v", err)
	}

	// 1. Test ChaCha20 encryption with magic byte 0x01
	ciphertext, err := encrypt_decrypt.EncryptChaCha(sharedKey, innerBytes)
	if err != nil {
		t.Fatalf("encryption failed: %v", err)
	}
	wirePayload := append([]byte{0x01}, ciphertext...)

	// Simulate receiving peer
	magic := wirePayload[0]
	if magic != 0x01 {
		t.Fatalf("expected magic 0x01, got %v", magic)
	}
	decrypted, err := encrypt_decrypt.DecryptChaCha(sharedKey, wirePayload[1:])
	if err != nil {
		t.Fatalf("decryption failed: %v", err)
	}

	var receivedMsg PersonalMessageProto
	if err := proto.Unmarshal(decrypted, &receivedMsg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if receivedMsg.Text != "Checkmate!" {
		t.Fatalf("expected text 'Checkmate!', got '%s'", receivedMsg.Text)
	}

	// 2. Test Plain payload with magic byte 0x00
	plainPayload := append([]byte{0x00}, innerBytes...)
	if plainPayload[0] != 0x00 {
		t.Fatalf("expected magic 0x00, got %v", plainPayload[0])
	}
	var receivedPlain PersonalMessageProto
	if err := proto.Unmarshal(plainPayload[1:], &receivedPlain); err != nil {
		t.Fatalf("failed to unmarshal plain: %v", err)
	}
	if receivedPlain.Text != "Checkmate!" {
		t.Fatalf("expected text 'Checkmate!', got '%s'", receivedPlain.Text)
	}
}

func TestMessenger_SharedKeyRegistry(t *testing.T) {
	broker := &MessageBroker{
		sharedKeys: make(map[types.PeerID][]byte),
	}

	var peer types.PeerID
	copy(peer[:], []byte("peer-12345678901234567890123456789012"))

	key := bytes.Repeat([]byte{0x99}, 32)
	broker.RegisterSharedKey(peer, key)

	retrieved, found := broker.GetSharedKey(peer)
	if !found {
		t.Fatalf("expected shared key to be found")
	}
	if !bytes.Equal(retrieved, key) {
		t.Fatalf("retrieved key mismatch")
	}
}
