package app_test

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/files"
)

func attWithRoot(o files.Offer, b byte) files.Offer {
	o.Root = [32]byte{}
	o.Root[0] = b
	return o
}

func attWithSize(o files.Offer, n uint64) files.Offer {
	o.Size = n
	return o
}

// Where an offered file belongs is the log's word, and only the log's: the author, the root, the size
// and the right to post in the channel are checked afresh. These tests ask attOf directly, so a card
// already made for the file id cannot deduplicate the offer and hide a missing check (T11.3e-1 Т-2).
func TestGroupFileRuleIsTheLogsNotACard(t *testing.T) {
	w, ns, g1 := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	if _, err := alice.app.GroupChannelSave(ctx, g1, convlog.ChannelSpec{ID: "dev", Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	path, _ := writeTemp(t, 1000)
	tr, err := alice.app.SendGroupFile(ctx, g1, "dev", "", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = alice.app.CancelFile(tr.ID) })
	w.drain()
	fidB, _ := hex.DecodeString(tr.ID)
	var fid [16]byte
	copy(fid[:], fidB)
	share, ok := alice.hold.share(fid)
	if !ok {
		t.Fatal("the author does not share the file")
	}
	good := share.Offer

	// the author's own offer: taken and placed in the channel the log names
	group, channel, thread, found, ok := app.AttOfForTest(bob.app, ctx, alice.node, good)
	if !found || !ok || group != g1 || channel != "dev" || thread != "" {
		t.Fatalf("the author's offer: place %q/%q/%q found=%v ok=%v", group, channel, thread, found, ok)
	}

	cases := []struct {
		name string
		from [32]byte
		o    files.Offer
	}{
		{"another sender claims the author's file", carol.node, func() files.Offer { o := good; o.Sender = carol.node; return o }()},
		{"another root", alice.node, attWithRoot(good, 9)},
		{"another size", alice.node, attWithSize(good, good.Size+1)},
	}
	for _, c := range cases {
		group, channel, thread, found, ok := app.AttOfForTest(bob.app, ctx, c.from, c.o)
		if !found || ok {
			t.Errorf("%s: place %q/%q/%q found=%v ok=%v", c.name, group, channel, thread, found, ok)
		}
	}

	// the event has not arrived: neither the file nor its place is known
	ghost := attWithRoot(good, 9)
	ghost.FID[0] ^= 0xff
	if group, channel, thread, found, ok := app.AttOfForTest(bob.app, ctx, alice.node, ghost); found || ok {
		t.Errorf("a file no log holds: place %q/%q/%q found=%v ok=%v", group, channel, thread, found, ok)
	}
}

// A channel can become read-only after the file was posted: the offer is weighed by the rights of
// the channel now, not by the rights the author had when the message went in.
func TestAFileOfAChannelTheAuthorMayNoLongerPostInIsNotTaken(t *testing.T) {
	w, ns, g1 := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	if _, err := alice.app.GroupChannelSave(ctx, g1, convlog.ChannelSpec{ID: "news", Name: "news"}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	path, _ := writeTemp(t, 400)
	tr, err := bob.app.SendGroupFile(ctx, g1, "news", "", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bob.app.CancelFile(tr.ID) })
	w.drain()
	if _, err := alice.app.GroupChannelSave(ctx, g1, convlog.ChannelSpec{ID: "news", Name: "news", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	fidB, _ := hex.DecodeString(tr.ID)
	var fid [16]byte
	copy(fid[:], fidB)
	share, ok := bob.hold.share(fid)
	if !ok {
		t.Fatal("the author does not share the file")
	}
	if group, channel, thread, found, ok := app.AttOfForTest(carol.app, ctx, bob.node, share.Offer); !found || ok {
		t.Fatalf("a file of a read-only channel: place %q/%q/%q found=%v ok=%v", group, channel, thread, found, ok)
	}
}

// An offer that comes before the event of its file is one more offer from somebody who is not a
// contact: refused the same way, and no card appears (services.md §8).
func TestAnOfferBeforeItsEventIsRefused(t *testing.T) {
	w, ns, g1 := team(t, 3)
	alice, _, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	path, _ := writeTemp(t, 300)
	tr, err := alice.app.SendGroupFile(ctx, g1, "", "", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = alice.app.CancelFile(tr.ID) })
	// the event is still in the queue: carol's log does not hold it
	fidB, _ := hex.DecodeString(tr.ID)
	var fid [16]byte
	copy(fid[:], fidB)
	share, ok := alice.hold.share(fid)
	if !ok {
		t.Fatal("the author does not share the file")
	}
	o := share.Offer
	o.Sender = carol.node
	if err := carol.app.OnFileOffer(carol.node, o.Message()); !errors.Is(err, app.ErrBadOffer) {
		t.Fatalf("an offer with no event behind it: %v", err)
	}
	if got := incoming(carol); len(got) != 0 {
		t.Fatalf("an offer with no event made a card: %+v", got)
	}
	w.drain()
}
