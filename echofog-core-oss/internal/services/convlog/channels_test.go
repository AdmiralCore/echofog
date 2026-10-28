package convlog

import (
	"testing"
)

// A group's channels: who may change the group makes them, every member writes in them, each has
// its own history and reader; the main channel keeps everything without a channel. A rebuild keeps
// it all, and the channels come out the same on every device.
func TestChannelsOfAGroup(t *testing.T) {
	f, x := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("space")
	lf, lx := groupReplica(t, f, rules), groupReplica(t, x, rules)
	r := member(t, lf, c, roster(f, "owner", x, "member"))
	ingest(t, lx, c, r)

	mk, _, err := lf.AppendChannel(c, "dev", "dev", false, false)
	if err != nil {
		t.Fatal(err)
	}
	room, _, err := lf.AppendChannel(c, "lounge", "Lounge", true, false)
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, lx, c, mk, room)
	if _, _, err := lx.AppendChannel(c, "mine", "mine", false, false); err != ErrForbidden {
		t.Fatalf("a member made a channel: %v", err)
	}
	chans := lx.Channels(c)
	if len(chans) != 2 || chans[0].ID != "dev" || !chans[1].Voice {
		t.Fatalf("channels: %+v", chans)
	}

	main, _ := say(t, lf, c, "in main")
	inDev, devID, err := lx.AppendMessageIn(c, "dev", "in dev", 0, "", EventID{}, EventID{})
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, lf, c, inDev)
	ingest(t, lx, c, main)

	mainPage, _, _ := lf.History(c, "", 50)
	devPage, _, _ := lf.HistoryIn(c, "dev", "", 50)
	if len(mainPage) != 1 || mainPage[0].Text != "in main" || len(devPage) != 1 || devPage[0].Channel != "dev" {
		t.Fatalf("main %v dev %v", mainPage, devPage)
	}
	meta, _ := lf.Meta(c)
	if meta.Chans["dev"].Unread != 1 || meta.UnreadAll() != 1 {
		t.Fatalf("unread: main %d dev %+v", meta.Unread, meta.Chans["dev"])
	}
	if err := lf.MarkReadIn(c, "dev", devID); err != nil {
		t.Fatal(err)
	}
	meta, _ = lf.Meta(c)
	if meta.UnreadAll() != 0 {
		t.Fatalf("still unread: %d", meta.UnreadAll())
	}

	// rename and remove: the newest write wins
	ren, _, _ := lf.AppendChannel(c, "dev", "development", false, false)
	rmRoom, _, _ := lf.AppendChannel(c, "lounge", "", true, true)
	ingest(t, lx, c, rmRoom, ren)
	chans = lx.Channels(c)
	if len(chans) != 1 || chans[0].Name != "development" {
		t.Fatalf("after rename and removal: %+v", chans)
	}

	lf.mu.Lock()
	_, err = lf.rebuildLocked(c, lf.bc(c.ID))
	lf.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	devPage, _, _ = lf.HistoryIn(c, "dev", "", 50)
	if len(devPage) != 1 || len(lf.Channels(c)) != 1 {
		t.Fatalf("after a rebuild: %v %v", devPage, lf.Channels(c))
	}
}
