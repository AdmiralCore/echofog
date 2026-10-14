package messenger

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
)

// Database key structure for lazy loading:
// <2 bytes "m:"> + <4 bytes AppID> + <8 bytes ChatID> + <8 bytes InvTime> + <8 bytes MsgID>
// 30 bytes

const (
	AppChatID uint32 = iota
	AppAiID
)

type MessageKey struct {
	AppID       uint32
	LocalChatID uint64
	// For quick search we should do this: MaxUint64 - ActualTimestamp
	InversionTimestamp uint64
	LocalMessageID     uint64
}

const MaxUint64 uint64 = 1<<64 - 1

func NewMessageKey(
	appID uint32,
	LocalChatID uint64,
	LocalMessageID uint64,
) *MessageKey {
	timestamp := MaxUint64 - uint64(time.Now().Unix())
	return &MessageKey{
		AppID:              appID,
		LocalChatID:        LocalChatID,
		InversionTimestamp: timestamp,
		LocalMessageID:     LocalMessageID,
	}
}

func (m *MessageKey) ToKey() [30]byte {
	var finalKey [30]byte
	copy(finalKey[:2], []byte("m:"))
	binary.BigEndian.PutUint32(finalKey[2:6], m.AppID)
	binary.BigEndian.PutUint64(finalKey[6:14], m.LocalChatID)
	binary.BigEndian.PutUint64(finalKey[14:22], m.InversionTimestamp)
	binary.BigEndian.PutUint64(finalKey[22:30], m.LocalMessageID)
	return finalKey
}

func GenerateLocalChatID(myID, targetID string, isGroup bool) uint64 {
	var globalChatStr string

	if isGroup {
		globalChatStr = "group:" + targetID
	} else {
		participants := []string{myID, targetID}
		sort.Strings(participants)
		globalChatStr = "dm:" + participants[0] + "_" + participants[1]
	}

	hash := sha256.Sum256([]byte(globalChatStr))

	return binary.BigEndian.Uint64(hash[:8])
}

type storageHelper struct {
	storage storage.MessageStorage
}

func newStorageHelper(storage storage.MessageStorage) *storageHelper {
	return &storageHelper{
		storage: storage,
	}
}

func (s *storageHelper) getNextID(chatID uint64) (uint64, error) {
	return s.storage.GetNextID(chatID)
}
