package messenger

import (
	"encoding/binary"
	"fmt"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/dto"
	"google.golang.org/protobuf/proto"
)

type MessageKeeper struct {
	Getter *MessageGetter
	Setter *MessageSetter
}

func NewMessageKeeper(mesSt storage.MessageStorage) *MessageKeeper {
	helper := newStorageHelper(mesSt)
	return &MessageKeeper{
		Getter: &MessageGetter{storage: mesSt, storageHelper: helper},
		Setter: &MessageSetter{storage: mesSt, storageHelper: helper},
	}
}

type MessageGetter struct {
	storage       storage.MessageStorage
	storageHelper *storageHelper
}

type MessageSetter struct {
	storage       storage.MessageStorage
	storageHelper *storageHelper
}

func (s *MessageSetter) SaveMessage(appID uint32, chatID uint64, msgProto *PersonalMessageProto) error {
	chatID = storage.BlindChat(s.storage, chatID) // who a chat is with does not show in the keys
	nextID, err := s.storageHelper.getNextID(chatID)
	if err != nil {
		return fmt.Errorf("failed to get next ID: %w", err)
	}

	data, err := proto.Marshal(msgProto)
	if err != nil {
		return fmt.Errorf("failed to marshal proto: %w", err)
	}

	key := NewMessageKey(appID, chatID, nextID)
	keyBytes := key.ToKey()

	return s.storage.Set(keyBytes[:], data)
}

func (g *MessageGetter) GetMessages(appID uint32, chatID uint64, limit int, beforeTimestamp uint64, myPeerID string, targetPeerID string) ([]dto.MessageDTO, error) {
	chatID = storage.BlindChat(g.storage, chatID)
	prefix := make([]byte, 14)
	copy(prefix[:2], []byte("m:"))
	binary.BigEndian.PutUint32(prefix[2:6], appID)
	binary.BigEndian.PutUint64(prefix[6:14], chatID)

	var startKey []byte

	if beforeTimestamp > 0 {
		invTime := MaxUint64 - beforeTimestamp + 1
		startKey = make([]byte, 22)
		copy(startKey[:14], prefix)
		binary.BigEndian.PutUint64(startKey[14:22], invTime)
	} else {
		startKey = prefix
	}

	entries, err := g.storage.FindValuesSeek(prefix, startKey, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch messages: %w", err)
	}

	result := make([]dto.MessageDTO, 0, len(entries))

	for _, entry := range entries {
		var protoMsg PersonalMessageProto
		if err = proto.Unmarshal(entry.Value, &protoMsg); err != nil {
			continue
		}

		dtoMsg := ProtoToDTO(&protoMsg, myPeerID, targetPeerID)
		result = append(result, dtoMsg)
	}

	return result, nil
}
