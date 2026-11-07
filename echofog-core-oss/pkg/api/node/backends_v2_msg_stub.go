package node

// Stubs of the T11.3d red commit; the implementation replaces this file.

func (b *v2MsgBackend) ChatNotify(ref ChatRef) (string, error)       { return "", ErrV2NotBuilt }
func (b *v2MsgBackend) SetChatNotify(ref ChatRef, mode string) error { return ErrV2NotBuilt }
func (b *v2MsgBackend) ChatFollow(ref ChatRef, root string, on bool) error {
	return ErrV2NotBuilt
}
func (b *v2MsgBackend) ChatFollowing(ref ChatRef) ([]string, error) { return nil, ErrV2NotBuilt }
func (b *v2MsgBackend) GroupVoice(groupID, channel string) ([]VoiceMemberDTO, error) {
	return nil, ErrV2NotBuilt
}
