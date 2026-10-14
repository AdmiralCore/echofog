package messenger

import (
	"github.com/echofog/echofog-core/pkg/dto"
)

func ProtoToDTO(msg *PersonalMessageProto, myPeerID string, targetID string) dto.MessageDTO {
	appID := ""
	if msg.Metadata != "" {
		meta := DecodeMetadata(msg.Metadata)
		appID = meta.AppID
	}
	return dto.MessageDTO{
		ID:          msg.Id,
		IsMine:      msg.SenderId == myPeerID,
		SenderID:    msg.SenderId,
		TargetID:    targetID,
		Type:        dto.MessageType(msg.Type),
		Text:        msg.Text,
		ContentHash: msg.ContentHash,
		Metadata:    msg.Metadata,
		Timestamp:   msg.Timestamp,
		ReplyToID:   msg.ReplyToId,
		AppID:       appID,
	}
}
