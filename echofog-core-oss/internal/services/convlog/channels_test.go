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

	mk, _, err := lf.AppendChannel(c, ChannelSpec{ID: "dev", Name: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	room, _, err := lf.AppendChannel(c, ChannelSpec{ID: "lounge", Name: "Lounge", Voice: true, Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, lx, c, mk, room)
	if _, _, err := lx.AppendChannel(c, ChannelSpec{ID: "mine", Name: "mine"}); err != ErrForbidden {
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
	ren, _, _ := lf.AppendChannel(c, ChannelSpec{ID: "dev", Name: "development", Topic: "builds"})
	rmRoom, _, _ := lf.AppendChannel(c, ChannelSpec{ID: "lounge", Voice: true, Remove: true})
	ingest(t, lx, c, rmRoom, ren)
	chans = lx.Channels(c)
	if len(chans) != 1 || chans[0].Name != "development" || chans[0].Topic != "builds" {
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

// An announcements channel: only those who may manage the group post in it; members comment. A
// thread can be named by its root's author, and the channel lists its threads.
func TestReadOnlyChannelAndThreads(t *testing.T) {
	f, x := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("space2")
	lf, lx := groupReplica(t, f, rules), groupReplica(t, x, rules)
	r := member(t, lf, c, roster(f, "owner", x, "member"))
	ingest(t, lx, c, r)
	news, _, err := lf.AppendChannel(c, ChannelSpec{ID: "news", Name: "news", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, lx, c, news)
	if _, _, err := lx.AppendMessageIn(c, "news", "from a member", 0, "", EventID{}, EventID{}); err != ErrForbidden {
		t.Fatalf("a member posted in a read-only channel: %v", err)
	}
	post, postID, err := lf.AppendMessageIn(c, "news", "release 1.0", 0, "", EventID{}, EventID{})
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, lx, c, post)
	comment, _, err := lx.AppendMessageIn(c, "news", "congrats", 0, "", EventID{}, postID)
	if err != nil {
		t.Fatalf("a member cannot comment: %v", err)
	}
	ingest(t, lf, c, comment)
	if _, _, err := lx.AppendThreadTitle(c, postID, "not mine"); err != ErrForbidden {
		t.Fatalf("a member named somebody else's thread: %v", err)
	}
	title, _, err := lf.AppendThreadTitle(c, postID, "Release talk")
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, lx, c, title)
	threads := lx.Threads(c, "news", 100)
	if len(threads) != 1 || threads[0].ThreadTitle != "Release talk" || threads[0].Replies != 1 {
		t.Fatalf("threads: %+v", threads)
	}
}

// A file posted in a channel or a thread travels the old way (a legacy message) and lands there,
// also after a rebuild; one that names a channel the log does not have lands in the main one.
func TestLegacyMessageInAChannelOrThread(t *testing.T) {
	f, x := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("space")
	lf := groupReplica(t, f, rules)
	member(t, lf, c, roster(f, "owner", x, "member"))
	if _, _, err := lf.AppendChannel(c, ChannelSpec{ID: "photos", Name: "photos"}); err != nil {
		t.Fatal(err)
	}
	_, root, err := lf.AppendMessageIn(c, "photos", "look", 0, "", EventID{}, EventID{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lf.AddLegacy(c, []Legacy{
		{ID: "f1", Author: x.id, TS: 1, Text: "cat.jpg", Channel: "photos"},
		{ID: "f2", Author: x.id, TS: 2, Text: "dog.jpg", Thread: root.String()},
		{ID: "f3", Author: x.id, TS: 3, Text: "lost.jpg", Channel: "nowhere"},
	}, true); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		ch, _, _ := lf.HistoryIn(c, "photos", "", 50)
		main, _, _ := lf.History(c, "", 50)
		th, _, _ := lf.Thread(c, root, "", 50)
		texts := func(ms []Message) (out []string) {
			for _, m := range ms {
				out = append(out, m.Text)
			}
			return
		}
		if got := texts(ch); len(got) != 2 || !contains(got, "cat.jpg") || !contains(got, "look") {
			t.Fatalf("%s: channel %v", when, got)
		}
		if got := texts(main); len(got) != 1 || got[0] != "lost.jpg" {
			t.Fatalf("%s: main %v", when, got)
		}
		if got := texts(th); len(got) != 1 || got[0] != "dog.jpg" {
			t.Fatalf("%s: thread %v", when, got)
		}
		if m, _ := lf.Message(c, root); m.Replies != 1 {
			t.Fatalf("%s: root has %d replies", when, m.Replies)
		}
	}
	check("live")
	lf.mu.Lock()
	_, err = lf.rebuildLocked(c, lf.bc(c.ID))
	lf.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	check("after a rebuild")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
