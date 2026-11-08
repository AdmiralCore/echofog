package app_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/files"
	"google.golang.org/protobuf/proto"
)

// ---- T11.3e-1: a file in a group is an attachment of the group's log; a group is closed by its owner -------------

func writeTemp(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	_, _ = rand.Read(data)
	p := filepath.Join(t.TempDir(), "план.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, data
}

// signedBy signs an event of kind with body in group g as node n, as a node whose own log would refuse it.
func signedBy(t *testing.T, n *gNode, g string, kind uint32, seq, lamport uint64, body proto.Message, channel string) []byte {
	t.Helper()
	var b []byte
	if body != nil {
		var err error
		if b, err = proto.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	var ext []byte
	if channel != "" {
		ext, _ = proto.Marshal(&convlog.EventExt{Channel: channel})
	}
	cid := convlog.Group(g).ID
	e := &convlog.Event{Conv: cid[:], Author: n.pub[:], Seq: seq, Lamport: lamport, TsMs: 1, Kind: kind, Body: b, Ext: ext}
	if err := convlog.Sign(e, func(m []byte) ([]byte, error) { return ed25519.Sign(n.priv, m), nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := convlog.Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func attBody(fid byte, text string) *convlog.MsgBody {
	return &convlog.MsgBody{Text: text, Att: &convlog.Attachment{Fid: bytes.Repeat([]byte{fid}, 16), Size: 100,
		MerkleRoot: bytes.Repeat([]byte{fid + 1}, 32), Name: "x.bin", Mime: "application/octet-stream"}}
}

func offerFor(sender *gNode, fid byte) *chatpb.PersonalMessageProto {
	o := files.Offer{Size: 100, ChunkSize: files.ChunkSize, Name: "x.bin", Mime: "application/octet-stream", Sender: sender.node}
	copy(o.FID[:], bytes.Repeat([]byte{fid}, 16))
	copy(o.Root[:], bytes.Repeat([]byte{fid + 1}, 32))
	return o.Message()
}

func incoming(n *gNode) []app.Transfer {
	l, _ := n.app.ListFileTransfers("")
	var out []app.Transfer
	for _, t := range l {
		if t.Dir == app.DirIn {
			out = append(out, t)
		}
	}
	return out
}

func accepted(t *testing.T, n *gNode, id string) app.Transfer {
	t.Helper()
	if _, err := n.app.AcceptFile(id); err != nil {
		t.Fatalf("AcceptFile: %v", err)
	}
	var got app.Transfer
	until(t, "the file to arrive", func() bool {
		got, _ = n.app.GetFileTransfer(id)
		return got.State == app.FileDone
	})
	return got
}

func TestGroupFileGoesToTheGroupChannelAndThreadTheSenderChose(t *testing.T) {
	w, ns, g1 := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	// a second group the three of them... alice and bob share: the old rule put a file in the first one
	g2, err := alice.app.CreateGroup(ctx, "Другая", "", app.GroupPrivate)
	if err != nil {
		t.Fatal(err)
	}
	inv, _ := alice.app.GroupInviteCreate(ctx, g2.ID, app.InviteSpec{TTL: time.Hour})
	if _, err := bob.app.JoinGroup(ctx, inv.Link); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if _, err := alice.app.GroupChannelSave(ctx, g1, convlog.ChannelSpec{ID: "dev", Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	root := send(t, bob, gref(g1, "dev"), "корень треда")
	w.drain()

	path, data := writeTemp(t, 3*files.ChunkSize/2)
	tr, err := alice.app.SendGroupFile(ctx, g1, "dev", root.ID, path)
	if err != nil {
		t.Fatalf("SendGroupFile: %v", err)
	}
	if tr.Group != g1 || tr.Channel != "dev" || tr.Thread != root.ID || tr.Dir != app.DirOut || tr.Name != "план.bin" {
		t.Fatalf("transfer %+v", tr)
	}
	w.drain()
	if w.fileOffers != 0 {
		t.Fatalf("%d personal offers were sent: the event is the offer", w.fileOffers)
	}
	for name, n := range map[string]*gNode{"bob": bob, "carol": carol} {
		in := incoming(n)
		if len(in) != 1 || in[0].Group != g1 || in[0].Channel != "dev" || in[0].Thread != root.ID || in[0].Peer != alice.nodeHex() ||
			in[0].State != app.FileOffered || in[0].ID != tr.ID || in[0].Size != int64(len(data)) {
			t.Fatalf("%s: incoming %+v", name, in)
		}
		// the card is in the thread of the channel the sender chose, with the file id the transfer has
		page, err := n.app.ChatThread(ctx, gref(g1, "dev"), root.ID, "", 20)
		if err != nil {
			t.Fatal(err)
		}
		var card *convlog.Message
		for i := range page.Messages {
			if page.Messages[i].File != nil {
				card = &page.Messages[i]
			}
		}
		if card == nil || card.Kind != "file" || card.File.ID != tr.ID || card.File.Name != "план.bin" || card.Channel != "dev" || card.Thread != root.ID {
			t.Fatalf("%s: card %+v in %+v", name, card, page.Messages)
		}
		got := accepted(t, n, tr.ID)
		if b, err := os.ReadFile(got.Path); err != nil || !bytes.Equal(b, data) {
			t.Fatalf("%s: the bytes differ: %v", name, err)
		}
	}
	// it is not in the other group, nor in the main channel
	if page, _ := bob.app.ChatHistory(ctx, gref(g2.ID, ""), "", 20); len(page.Messages) != 0 {
		t.Fatalf("the file appeared in another group: %+v", page.Messages)
	}
	if page, _ := bob.app.ChatHistory(ctx, gref(g1, ""), "", 20); len(page.Messages) != 0 {
		t.Fatalf("the file appeared in the main channel: %+v", page.Messages)
	}
}

func TestGroupFileOfferIsKeptToTheRuleOfTheLog(t *testing.T) {
	w, ns, g1 := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	path, _ := writeTemp(t, 1000)
	tr, err := alice.app.SendGroupFile(ctx, g1, "", "", path)
	if err != nil {
		t.Fatal(err)
	}
	w.drain()
	in := incoming(bob)
	if len(in) != 1 {
		t.Fatalf("bob: %+v", in)
	}
	fid, _ := hex.DecodeString(tr.ID)
	// somebody else offers the same file id: the log names alice as its author
	o := files.Offer{Size: uint64(tr.Size), ChunkSize: files.ChunkSize, Name: tr.Name, Sender: carol.node}
	copy(o.FID[:], fid)
	if share, ok := alice.hold.share(o.FID); ok {
		o.Root = share.Offer.Root
	}
	bob.app.OnIncoming(ctx, carol.node, o.Message())
	if got := incoming(bob); len(got) != 1 || got[0].Peer != alice.nodeHex() {
		t.Fatalf("an offer of another sender for the same file: %+v", got)
	}
	// a file id the log does not hold, from a member who is no contact
	bob.app.OnIncoming(ctx, carol.node, offerFor(carol, 0x77))
	if got := incoming(bob); len(got) != 1 {
		t.Fatalf("an offer with no event behind it was taken: %+v", got)
	}
	// the same offer in the form of a plain message from a non-contact never makes a group file
	bob.app.OnIncoming(ctx, alice.node, offerFor(alice, 0x55))
	if got := incoming(bob); len(got) != 1 {
		t.Fatalf("an offer of a file the author never posted: %+v", got)
	}
}

func TestAFileOfAStrangerOrOfAnAuthorWhoMayNotPostIsNotShown(t *testing.T) {
	w, ns, g1 := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	if _, err := alice.app.GroupChannelSave(ctx, g1, convlog.ChannelSpec{ID: "news", Name: "news", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	path, _ := writeTemp(t, 500)
	// the sender's own app refuses a channel where it may not post
	if _, err := bob.app.SendGroupFile(ctx, g1, "news", "", path); !errors.Is(err, app.ErrNoRight) {
		t.Fatalf("a read-only channel: %v", err)
	}
	// a stranger in none of the groups: the event is not taken, and its offer is not either
	w2, nd := newGroupWorld(t, 1)
	_ = w2
	dave := nd[0]
	w.pubs[dave.node] = dave.pub
	w.nodes[dave.node] = dave
	strangerEv := signedBy(t, dave, g1, convlog.KindMsg, 1, 500, attBody(0x31, "из ниоткуда"), "")
	carol.app.OnIncoming(ctx, dave.node, &chatpb.PersonalMessageProto{ConvEvent: strangerEv})
	carol.app.OnIncoming(ctx, dave.node, offerFor(dave, 0x31))
	if got := incoming(carol); len(got) != 0 {
		t.Fatalf("a stranger's file: %+v", got)
	}
	// a member who may not write in the channel forges the event and offers the file
	forged := signedBy(t, bob, g1, convlog.KindMsg, 1, 600, attBody(0x41, "в новости"), "news")
	carol.app.OnIncoming(ctx, bob.node, &chatpb.PersonalMessageProto{ConvEvent: forged})
	carol.app.OnIncoming(ctx, bob.node, offerFor(bob, 0x41))
	if got := incoming(carol); len(got) != 0 {
		t.Fatalf("a file from a read-only channel: %+v", got)
	}
	if page, _ := carol.app.ChatHistory(ctx, gref(g1, "news"), "", 20); len(page.Messages) != 0 {
		t.Fatalf("the forged message is shown: %+v", page.Messages)
	}
	// a read-only member of an announcements channel: the owner may post there
	if _, err := alice.app.SendGroupFile(ctx, g1, "news", "", path); err != nil {
		t.Fatalf("the owner in the announcements channel: %v", err)
	}
}

// the author serves the bytes only to a member of the conversation that holds the event (services.md §8)
func TestFileBytesAreServedToMembersOnly(t *testing.T) {
	w, ns, g1 := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	wd, nd := newGroupWorld(t, 1)
	_ = wd
	dave := nd[0]
	w.pubs[dave.node] = dave.pub
	path, _ := writeTemp(t, 1000)
	tr, err := alice.app.SendGroupFile(ctx, g1, "", "", path)
	if err != nil {
		t.Fatal(err)
	}
	w.drain()
	fidB, _ := hex.DecodeString(tr.ID)
	var fid [16]byte
	copy(fid[:], fidB)
	sh, ok := alice.hold.share(fid)
	if !ok || sh.Authorize == nil {
		t.Fatalf("the share has no rule: %v", ok)
	}
	if !sh.Authorize(bob.node) || !sh.Authorize(carol.node) {
		t.Fatal("a member was refused")
	}
	if sh.Authorize(dave.node) || sh.Authorize([32]byte{9}) {
		t.Fatal("a stranger was served")
	}
	// removed from the group: no more
	if err := alice.app.GroupRemove(ctx, g1, carol.nodeHex(), false); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if sh.Authorize(carol.node) {
		t.Fatal("a removed member was served")
	}
	if !sh.Authorize(bob.node) {
		t.Fatal("the member that stays was refused")
	}
	// the message deleted: the file goes with it
	m := incoming(bob)[0]
	page, _ := alice.app.ChatHistory(ctx, gref(g1, ""), "", 20)
	var msgID string
	for _, x := range page.Messages {
		if x.File != nil && x.File.ID == m.ID {
			msgID = x.ID
		}
	}
	if msgID == "" {
		t.Fatal("no card")
	}
	if err := alice.app.ChatDelete(ctx, gref(g1, ""), msgID); err != nil {
		t.Fatal(err)
	}
	if sh.Authorize(bob.node) {
		t.Fatal("a deleted file was served")
	}
}

// the file of a contact keeps working the old way
func TestAFileToAContactIsStillAPersonalOffer(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	path, _ := a.file("x.bin", 1000)
	if _, err := a.app.SendFile(context.Background(), b.hex(), path); err != nil {
		t.Fatal(err)
	}
	if err := offerTo(t, a, b); err != nil {
		t.Fatalf("a contact's offer: %v", err)
	}
}
