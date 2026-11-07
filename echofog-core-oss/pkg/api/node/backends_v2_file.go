package node

import (
	"encoding/hex"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/pkg/dto"
)

// The protocol 2 files domain (T11.3d): the methods of fileBackend over the app layer, with the DTO of
// v1 (dto.FileTransferDTO). The event is the one the desktop already emits (file:transfer).

func transferDTO(t app.Transfer) dto.FileTransferDTO {
	return dto.FileTransferDTO{ID: t.ID, Peer: t.Peer, Dir: t.Dir, Name: t.Name, Size: t.Size, Mime: t.Mime, State: t.State, Error: t.Error,
		Path: t.Path, Done: t.Done, Rate: t.Rate, Stalled: t.Stalled, Group: t.Group, Members: t.Members, Delivered: t.Delivered,
		Created: t.Created.UnixMilli(), Updated: t.Updated.UnixMilli()}
}

func (b *v2MsgBackend) AcceptFile(id string) (dto.FileTransferDTO, error) {
	t, err := b.app.AcceptFile(id)
	if err != nil {
		return dto.FileTransferDTO{}, v2Err(err)
	}
	return transferDTO(t), nil
}

func (b *v2MsgBackend) DeclineFile(id string) error { return v2Err(b.app.DeclineFile(id)) }

func (b *v2MsgBackend) CancelFile(id string) error { return v2Err(b.app.CancelFile(id)) }

func (b *v2MsgBackend) GetFileTransfer(id string) (dto.FileTransferDTO, error) {
	t, err := b.app.GetFileTransfer(id)
	if err != nil {
		return dto.FileTransferDTO{}, v2Err(err)
	}
	return transferDTO(t), nil
}

func (b *v2MsgBackend) ListFileTransfers(peerIDHex string) ([]dto.FileTransferDTO, error) {
	list, err := b.app.ListFileTransfers(peerIDHex)
	if err != nil {
		return nil, v2Err(err)
	}
	out := make([]dto.FileTransferDTO, 0, len(list))
	for _, t := range list {
		out = append(out, transferDTO(t))
	}
	return out, nil
}

func (b *v2MsgBackend) FileTransferPath(id string) (string, error) {
	p, err := b.app.FileTransferPath(id)
	return p, v2Err(err)
}

func (b *v2MsgBackend) DownloadDir() string { return b.app.DownloadDir() }

// OnFileTransfer calls cb whenever a transfer changes; the returned function stops it.
func (b *v2MsgBackend) OnFileTransfer(cb func(dto.FileTransferDTO)) func() {
	if cb == nil {
		return func() {}
	}
	return b.app.OnFileTransfer(func(t app.Transfer) { cb(transferDTO(t)) })
}

// SendFile offers a file to a contact. The argument is the contact's node ID (the key of its account
// is taken too, as v1 took it).
func (b *v2MsgBackend) SendFile(pubKeyHex string, path string) (dto.FileTransferDTO, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	var t app.Transfer
	err := asContact(pubKeyHex, func(node string) (err error) {
		t, err = b.app.SendFile(ctx, node, path)
		return err
	})
	if err != nil {
		return dto.FileTransferDTO{}, v2Err(err)
	}
	return transferDTO(t), nil
}

// SendGroupFile offers a file to the members of a group. The offer reaches each member as a file
// message; the channel and the thread only place the answer for the screen, the offer itself has no
// field for them.
func (b *v2MsgBackend) SendGroupFile(groupID, channel, thread, path string) (GroupMessageDTO, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	ms, err := b.app.GroupMembers(groupID)
	if err != nil {
		return GroupMessageDTO{}, v2Err(err)
	}
	var to [][32]byte
	var self string
	for _, m := range ms {
		if m.Self {
			self = m.Node
			continue
		}
		raw, derr := hex.DecodeString(m.Node)
		if derr != nil || len(raw) != 32 {
			continue
		}
		to = append(to, [32]byte(raw))
	}
	t, err := b.app.SendGroupFile(ctx, groupID, to, path)
	if err != nil {
		return GroupMessageDTO{}, v2Err(err)
	}
	return GroupMessageDTO{ID: t.ID, GroupID: groupID, Sender: self, Text: t.Name, Timestamp: t.Created.UnixMilli(), IsMine: true, Verified: true,
		File: &GroupFileDTO{ID: t.ID, Name: t.Name, Size: t.Size, Mime: t.Mime, Channel: channel, Thread: thread}}, nil
}
