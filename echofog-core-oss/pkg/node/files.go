package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/filexfer"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/dto"
)

// TopicFileTransfer is the event bus topic of transfer updates (payload: filexfer.Info).
const TopicFileTransfer = "file:transfer"

// fileLink sends transfer frames directly when the peer is connected, else over a relay circuit
// (opening one if needed). The frames are sealed end to end, so the relay learns nothing.
type fileLink struct{ n *Node }

func (l fileLink) Send(ctx context.Context, peer types.PeerID, frame []byte) error {
	if l.n.RelayClient == nil {
		return l.n.Swarm.SendRawFrame(peer, net_types.TypeFileXfer, frame)
	}
	return l.n.RelayClient.SendUniversal(ctx, peer, net_types.TypeFileXfer, frame)
}

// DownloadDir is where received files go.
func (n *Node) DownloadDir() string {
	if d := n.Cfg.Storage.DownloadDir; d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(n.Cfg.Storage.DatabasePath, "downloads")
	}
	return filepath.Join(home, "Downloads", "EchoFog")
}

// startFiles sets up file transfers: frames from both paths reach the service, offers arrive as
// end-to-end encrypted chat messages, and downloads under way when the app closed continue.
func (n *Node) startFiles() error {
	n.Files = filexfer.New(filexfer.Options{
		Store:       n.Storage,
		Transport:   fileLink{n},
		DownloadDir: n.DownloadDir,
		Connect:     n.EnsureConnected,
		Logger:      n.Logger,
		Emit: func(i filexfer.Info) {
			if n.EventBus != nil {
				n.EventBus.Emit(TopicFileTransfer, i)
			}
		},
	})
	n.Swarm.RegisterFastFrameHandler(net_types.TypeFileXfer, func(payload []byte, _ types.PeerID) error {
		n.Files.HandleFrame(payload)
		return nil
	})
	if n.RelayClient != nil {
		n.RelayClient.AddDataListener(func(_ uint32, mt net_types.MessageType, payload []byte, _ types.PeerID) {
			if mt == net_types.TypeFileXfer {
				n.Files.HandleFrame(payload)
			}
		})
	}
	n.Messenger.AddHandleCallback(func(m *dto.MessageDTO) {
		if m == nil || m.IsMine || m.Type != dto.MsgTypeFile {
			return
		}
		from, err := types.PeerIDFromString(m.SenderID)
		if err != nil {
			return
		}
		offer, ok := OfferFromMetadata(m.Metadata)
		if !ok {
			return
		}
		if _, err := n.Files.OnOffer(from, offer); err != nil {
			n.Logger.Warn("FileXfer: ignoring a malformed file offer", "from", from.String()[:8], "err", err)
		}
	})
	return n.Files.Start()
}

// SendFile offers the file at path to peer: it is hashed, and the offer goes out as an end-to-end
// encrypted message (queued like any message when the peer is offline). The file itself moves once
// the peer accepts.
func (n *Node) SendFile(peer types.PeerID, path string) (filexfer.Info, error) {
	if n.Files == nil || n.Messenger == nil {
		return filexfer.Info{}, errors.New("node not started")
	}
	offer, err := n.Files.Prepare(peer, path)
	if err != nil {
		return filexfer.Info{}, err
	}
	meta := &messenger.MessageMetadata{
		FileName: offer.Name, FileSize: offer.Size, FileKeyHex: offer.Key, MimeType: offer.Mime,
		Custom: map[string]any{"fid": offer.ID, "root": offer.Root},
	}
	err = n.Messenger.SendChatMessageWithTier(peer, offer.Name, messenger.TierX3DH, messenger.SendOptions{
		Type: messenger.PersonalMessageTypeProto_MsgTypeFile, ContentHash: offer.Root, Metadata: meta,
	})
	if err != nil {
		n.Files.Withdraw(offer.ID)
		return filexfer.Info{}, err
	}
	return n.Files.Get(offer.ID)
}

// OfferFromMetadata reads a file offer out of a message's metadata.
func OfferFromMetadata(raw string) (filexfer.Offer, bool) {
	meta := messenger.DecodeMetadata(raw)
	fid, _ := meta.Custom["fid"].(string)
	root, _ := meta.Custom["root"].(string)
	if fid == "" || root == "" || meta.FileKeyHex == "" {
		return filexfer.Offer{}, false
	}
	return filexfer.Offer{ID: fid, Name: meta.FileName, Size: meta.FileSize, Mime: meta.MimeType, Root: root, Key: meta.FileKeyHex}, true
}
