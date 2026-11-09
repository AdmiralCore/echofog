package node

import (
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/dto"
)

// v1ChatBackend is the protocol 1 implementation of chatBackend: the code that used to sit in the
// NodeApi methods of this domain, moved here unchanged by T11.1c. The NodeApi methods below are
// one-line delegates, so at protocol 1 the bodies run exactly as before, and at protocol 2 the
// same delegates reach v2PendingBackend (see backends.go).
type v1ChatBackend struct{ *NodeApi }

// chatDomain is the chats backend of this facade, chosen the same way as netDomain.
func (n *NodeApi) chatDomain() chatBackend {
	if n.backends.chat != nil {
		return n.backends.chat
	}
	if n.node != nil && n.node.Cfg != nil && n.node.Cfg.ProtocolOrDefault() == config.ProtocolV2 {
		return v2PendingBackend{}
	}
	return &v1ChatBackend{n}
}

func (n *NodeApi) ChatAttachments(ref ChatRef) (ChatAttachments, error) {
	return n.chatDomain().ChatAttachments(ref)
}

func (n *NodeApi) ChatDelete(ref ChatRef, id string) error {
	return n.chatDomain().ChatDelete(ref, id)
}

func (n *NodeApi) ChatEdit(ref ChatRef, id, text string) error {
	return n.chatDomain().ChatEdit(ref, id, text)
}

func (n *NodeApi) ChatHistory(ref ChatRef, cursor string, limit int) (ChatPage, error) {
	return n.chatDomain().ChatHistory(ref, cursor, limit)
}

func (n *NodeApi) ChatMarkRead(ref ChatRef, id string) error {
	return n.chatDomain().ChatMarkRead(ref, id)
}

func (n *NodeApi) ChatMessage(ref ChatRef, id string) (ChatMessage, error) {
	return n.chatDomain().ChatMessage(ref, id)
}

func (n *NodeApi) ChatPin(ref ChatRef, id string, on bool) error {
	return n.chatDomain().ChatPin(ref, id, on)
}

func (n *NodeApi) ChatPins(ref ChatRef) ([]ChatMessage, error) {
	return n.chatDomain().ChatPins(ref)
}

func (n *NodeApi) ChatReact(ref ChatRef, id, emoji string, on bool) error {
	return n.chatDomain().ChatReact(ref, id, emoji, on)
}

func (n *NodeApi) ChatRows() ([]ConversationDTO, error) {
	return n.chatDomain().ChatRows()
}

func (n *NodeApi) ChatSend(ref ChatRef, req SendRequest) (ChatMessage, error) {
	return n.chatDomain().ChatSend(ref, req)
}

func (n *NodeApi) ChatThread(ref ChatRef, root, cursor string, limit int) (ChatPage, error) {
	return n.chatDomain().ChatThread(ref, root, cursor, limit)
}

func (n *NodeApi) Conversations() ([]ConversationDTO, error) {
	return n.chatDomain().Conversations()
}

func (n *NodeApi) GetMessages(contactID string, limit int, beforeTimestamp uint64) ([]dto.MessageDTO, error) {
	return n.chatDomain().GetMessages(contactID, limit, beforeTimestamp)
}

func (n *NodeApi) ListSavedMessages() ([]dto.SavedMessageDTO, error) {
	return n.chatDomain().ListSavedMessages()
}

func (n *NodeApi) MessageHandler() <-chan *dto.MessageDTO {
	return n.chatDomain().MessageHandler()
}

func (n *NodeApi) OnMessageState(cb func(MessageStateDTO)) {
	n.v2m.rememberState(cb)
	n.chatDomain().OnMessageState(cb)
}

func (n *NodeApi) ResetEncryptionSession(peerID string) error {
	return n.chatDomain().ResetEncryptionSession(peerID)
}

func (n *NodeApi) SaveMessage(m dto.SavedMessageDTO) (dto.SavedMessageDTO, error) {
	return n.chatDomain().SaveMessage(m)
}

func (n *NodeApi) SendMessage(pubKeyHex string, text string) error {
	return n.chatDomain().SendMessage(pubKeyHex, text)
}

func (n *NodeApi) SubscribeConversations(cb func(ConvChange)) {
	n.v2m.rememberConv(cb)
	n.chatDomain().SubscribeConversations(cb)
}

func (n *NodeApi) UnsaveMessage(id string) error {
	return n.chatDomain().UnsaveMessage(id)
}
