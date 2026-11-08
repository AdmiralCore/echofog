package app

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/internal/v2/files"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// A file in a group is the attachment of a message of the group's log (conversation-log.md §2.5): the
// message names the group, the channel and the thread, the bytes come over files/2 from the author. A node
// never believes what an offer says about where a file belongs: it reads that from its own log
// (services.md §8).

// authAsk is how long the directory may take to name the account behind a requester.
const authAsk = 3 * time.Second

// SendGroupFile posts the file at path in a group, to the channel and the thread the sender chose: a message
// with the file as its attachment goes to the members through the log; the bytes are served to the members
// who may read the message. The sender needs the right to post a file there.
func (a *App) SendGroupFile(ctx context.Context, group, channel, thread, path string) (Transfer, error) {
	if a.cfg.Files == nil {
		return Transfer{}, ErrFilesOff
	}
	_, cv, c, _, err := a.grp(group)
	if err != nil {
		return Transfer{}, err
	}
	l := cv.Log()
	root, err := parseEvent(thread)
	if err != nil {
		return Transfer{}, err
	}
	if l.Closed(c) {
		return Transfer{}, ErrGroupClosed
	}
	if !l.MayPost(c, l.Self(), channel, !root.IsZero(), true) {
		return Transfer{}, ErrNoRight
	}
	a.sweep()
	sh, src, err := a.prepare(path, nil)
	if err != nil {
		return Transfer{}, err
	}
	fid := sh.Offer.FID
	sh.Authorize = a.fileAuth(fid)
	att := &convlog.Attachment{Fid: fid[:], Size: sh.Offer.Size, MerkleRoot: sh.Offer.Root[:], Name: sh.Offer.Name, Mime: sh.Offer.Mime}
	// the message is made first: a file the log would refuse is never shared
	raw, _, err := l.AppendAttachment(c, channel, att, sh.Offer.Name, convlog.EventID{}, root)
	if err != nil {
		if src != nil {
			_ = src.Close()
		}
		switch {
		case errors.Is(err, convlog.ErrClosed):
			return Transfer{}, ErrGroupClosed
		case errors.Is(err, convlog.ErrBadBody):
			return Transfer{}, ErrBadFile
		}
		return Transfer{}, rightErr(err)
	}
	t, err := a.publish(ctx, sh, src, path, "", group, false, nil, channel, thread)
	if err != nil {
		return Transfer{}, err
	}
	a.publishGroup(ctx, cv, c, raw)
	return t, nil
}

// fileAuth is the rule by which the author serves the bytes of a file posted in a group: to a member of the
// conversation whose log holds the message with that file id, who may read the channel it is in. Asked at
// every request, so a member who was removed, or a message that was deleted, ends the serving.
func (a *App) fileAuth(fid [16]byte) func(node [32]byte) bool {
	return func(node [32]byte) bool {
		if a.cfg.Groups == nil || a.cfg.PubOf == nil || a.cfg.Convs == nil {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), authAsk)
		defer cancel()
		pub, err := a.cfg.PubOf(ctx, node)
		if err != nil || convsync.NodeOf(pub) != node {
			return false
		}
		peer := types.PeerPubKeyToID(pub)
		l := a.cfg.Convs.Log()
		for _, c := range a.cfg.Groups.SharedGroups(pub) {
			if _, ok := l.AttachmentIn(c, fid); !ok {
				continue
			}
			// every member reads every channel of the group (no right to read can be taken away)
			if l.Participant(c, peer) {
				return true
			}
		}
		return false
	}
}

// attOf looks in the logs of the groups this node and the sender are both in for the message that carries the
// offered file. found: some group's log holds a message with that file id; ok: it is the sender's message, it
// names the offered root and size, the sender takes part and may post a file in that channel. pl is where the
// file belongs.
func (a *App) attOf(ctx context.Context, from [32]byte, o files.Offer) (pl place, found, ok bool) {
	if a.cfg.Groups == nil || a.cfg.PubOf == nil || a.cfg.Convs == nil {
		return place{}, false, false
	}
	pub, err := a.cfg.PubOf(ctx, from)
	if err != nil || convsync.NodeOf(pub) != from {
		return place{}, false, false
	}
	peer := types.PeerPubKeyToID(pub)
	l := a.cfg.Convs.Log()
	for _, c := range a.cfg.Groups.SharedGroups(pub) {
		ref, has := l.AttachmentIn(c, o.FID)
		if !has {
			continue
		}
		found = true
		m := ref.Msg
		if m.Author != hex.EncodeToString(peer[:]) || ref.Root != o.Root || uint64(m.File.Size) != o.Size ||
			!l.Participant(c, peer) || !l.MayPost(c, peer, m.Channel, m.Thread != "", true) {
			continue
		}
		return place{group: c.Group, channel: m.Channel, thread: m.Thread}, true, true
	}
	return place{}, found, false
}

// offerFromEvent turns a message of a group's log that carries a file into an incoming transfer waiting for
// the user: the card the members see, also those who were offline when it was posted. The same rule as for
// an offer applies; a message that does not pass it makes nothing.
func (a *App) offerFromEvent(c convlog.Conv, m convlog.Message) {
	if a.cfg.FileNet == nil || c.Kind != KindGroup || m.Mine || m.Deleted || m.File == nil || m.File.Root == "" {
		return
	}
	node, ok := a.NodeOfPeer(m.Author)
	if !ok {
		return
	}
	from, ok := nodeOfHex(node)
	if !ok {
		return
	}
	fid, err1 := hex.DecodeString(m.File.ID)
	root, err2 := hex.DecodeString(m.File.Root)
	if err1 != nil || err2 != nil || len(fid) != 16 || len(root) != 32 || m.File.Size <= 0 {
		return
	}
	o := files.Offer{Size: uint64(m.File.Size), ChunkSize: files.ChunkSize, Name: m.File.Name, Mime: m.File.Mime, Sender: from}
	copy(o.FID[:], fid)
	copy(o.Root[:], root)
	if o.Validate() != nil {
		return
	}
	pub, err := a.cfg.PubOf(context.Background(), from)
	if err != nil || convsync.NodeOf(pub) != from {
		return
	}
	peer := types.PeerPubKeyToID(pub)
	l := a.cfg.Convs.Log()
	if hex.EncodeToString(peer[:]) != m.Author || !l.Participant(c, peer) || !l.MayPost(c, peer, m.Channel, m.Thread != "", true) {
		return
	}
	_, _ = a.addIncomingIn(from, place{group: c.Group, channel: m.Channel, thread: m.Thread}, o, nil)
}
