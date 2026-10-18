package node

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"

	"github.com/echofog/echofog-core/internal/services/filexfer"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/echofog/echofog-core/pkg/events"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

func fileDTO(i filexfer.Info) dto.FileTransferDTO {
	return dto.FileTransferDTO{
		ID: i.ID, Peer: i.Peer, Dir: string(i.Dir), Name: i.Name, Size: i.Size, Mime: i.Mime, State: string(i.State),
		Error: i.Error, Path: i.Path, Done: i.Done, Rate: i.Rate, Stalled: i.Stalled, Created: i.Created, Updated: i.Updated,
	}
}

func (n *NodeApi) files() (*filexfer.Service, error) {
	if n.node == nil || n.node.Files == nil {
		return nil, errors.New("node not started")
	}
	return n.node.Files, nil
}

// SendFile offers a file to the contact with this public key. It shows in the chat at once; its
// bytes move once the contact accepts.
func (n *NodeApi) SendFile(pubKeyHex string, path string) (dto.FileTransferDTO, error) {
	if _, err := n.files(); err != nil {
		return dto.FileTransferDTO{}, err
	}
	raw, err := hex.DecodeString(pubKeyHex)
	if err != nil {
		return dto.FileTransferDTO{}, err
	}
	var peer types.PeerID
	sum := sha256.Sum256(raw)
	copy(peer[:], sum[:])
	info, err := n.node.SendFile(peer, path)
	if err != nil {
		return dto.FileTransferDTO{}, err
	}
	return fileDTO(info), nil
}

// AcceptFile starts receiving an offered file into the download folder.
func (n *NodeApi) AcceptFile(id string) (dto.FileTransferDTO, error) {
	f, err := n.files()
	if err != nil {
		return dto.FileTransferDTO{}, err
	}
	info, err := f.Accept(id)
	if err != nil {
		return dto.FileTransferDTO{}, err
	}
	return fileDTO(info), nil
}

// DeclineFile turns an offered file down.
func (n *NodeApi) DeclineFile(id string) error {
	f, err := n.files()
	if err != nil {
		return err
	}
	return f.Decline(id)
}

// CancelFile stops a transfer either way.
func (n *NodeApi) CancelFile(id string) error {
	f, err := n.files()
	if err != nil {
		return err
	}
	return f.Cancel(id)
}

// GetFileTransfer is one transfer.
func (n *NodeApi) GetFileTransfer(id string) (dto.FileTransferDTO, error) {
	f, err := n.files()
	if err != nil {
		return dto.FileTransferDTO{}, err
	}
	info, err := f.Get(id)
	if err != nil {
		return dto.FileTransferDTO{}, err
	}
	return fileDTO(info), nil
}

// ListFileTransfers is every transfer with a contact (peer ID in hex), or all of them for "".
func (n *NodeApi) ListFileTransfers(peerIDHex string) ([]dto.FileTransferDTO, error) {
	f, err := n.files()
	if err != nil {
		return nil, err
	}
	var peer types.PeerID
	if peerIDHex != "" {
		if peer, err = types.PeerIDFromString(peerIDHex); err != nil {
			return nil, err
		}
	}
	list := f.List(peer)
	out := make([]dto.FileTransferDTO, 0, len(list))
	for _, i := range list {
		out = append(out, fileDTO(i))
	}
	return out, nil
}

// FileTransferPath is the file of a finished incoming transfer (or the source of an outgoing one),
// when it is still there: for "open" and "show in folder".
func (n *NodeApi) FileTransferPath(id string) (string, error) {
	f, err := n.files()
	if err != nil {
		return "", err
	}
	info, err := f.Get(id)
	if err != nil {
		return "", err
	}
	if info.Path == "" || (info.Dir == filexfer.Incoming && info.State != filexfer.StateDone) {
		return "", errors.New("the file is not there yet")
	}
	if _, err := os.Stat(info.Path); err != nil {
		return "", errors.New("the file was moved or deleted")
	}
	return info.Path, nil
}

// DownloadDir is where received files go.
func (n *NodeApi) DownloadDir() string {
	if n.node == nil {
		return ""
	}
	return n.node.DownloadDir()
}

// OnFileTransfer calls cb whenever a transfer changes (progress a few times a second).
func (n *NodeApi) OnFileTransfer(cb func(dto.FileTransferDTO)) func() {
	if n.node == nil || n.node.EventBus == nil {
		return func() {}
	}
	return n.node.EventBus.On(coreNode.TopicFileTransfer, func(ev events.Event) {
		if i, ok := ev.Payload.(filexfer.Info); ok {
			cb(fileDTO(i))
		}
	})
}

// scrubMessage drops the file key from a file message before it leaves the core: the UI never
// needs it, and it is the one secret of the transfer.
func scrubMessage(m dto.MessageDTO) dto.MessageDTO {
	if m.Type != dto.MsgTypeFile || m.Metadata == "" {
		return m
	}
	meta := messenger.DecodeMetadata(m.Metadata)
	meta.FileKeyHex = ""
	m.Metadata = messenger.EncodeMetadata(&meta)
	return m
}
