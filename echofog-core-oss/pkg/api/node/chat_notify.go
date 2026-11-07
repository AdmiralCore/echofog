package node

// The notification settings, the followed threads and the voice rooms of the protocol 2 chat (T7.7).
// They have no v1 counterpart, so they are not part of the domain backends: they exist only while
// the app layer is attached, and say ErrV2NotBuilt otherwise.

// VoiceMemberDTO is one member present in a voice room.
type VoiceMemberDTO struct {
	PeerID string `json:"peerId"`
	Muted  bool   `json:"muted"`
	Camera bool   `json:"camera,omitempty"`
	Stream bool   `json:"stream,omitempty"`
}

func (n *NodeApi) v2msg() (*v2MsgBackend, error) {
	n.v2m.mu.Lock()
	b := n.v2m.b
	n.v2m.mu.Unlock()
	if b == nil {
		return nil, ErrV2NotBuilt
	}
	return b, nil
}

// ChatNotify tells what a chat's channel tells the user about: "all", "mentions" or "none". The
// setting is local to this device.
func (n *NodeApi) ChatNotify(ref ChatRef) (string, error) {
	b, err := n.v2msg()
	if err != nil {
		return "", err
	}
	return b.ChatNotify(ref)
}

// SetChatNotify sets it.
func (n *NodeApi) SetChatNotify(ref ChatRef, mode string) error {
	b, err := n.v2msg()
	if err != nil {
		return err
	}
	return b.SetChatNotify(ref, mode)
}

// ChatFollow follows the thread under a message of a chat, or leaves it.
func (n *NodeApi) ChatFollow(ref ChatRef, root string, on bool) error {
	b, err := n.v2msg()
	if err != nil {
		return err
	}
	return b.ChatFollow(ref, root, on)
}

// ChatFollowing lists the roots of the threads the user follows in a chat.
func (n *NodeApi) ChatFollowing(ref ChatRef) ([]string, error) {
	b, err := n.v2msg()
	if err != nil {
		return nil, err
	}
	return b.ChatFollowing(ref)
}

// GroupVoice lists who is in a voice room of a group now (from the live sessions, never the log).
func (n *NodeApi) GroupVoice(groupID, channel string) ([]VoiceMemberDTO, error) {
	b, err := n.v2msg()
	if err != nil {
		return nil, err
	}
	return b.GroupVoice(groupID, channel)
}
