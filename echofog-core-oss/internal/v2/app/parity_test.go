package app_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
)

// ---- T11.3e-2: what is left of the chat of v1, without a change of the wire --------------------------------

// totalLetters is how many letters the world has carried so far.
func totalLetters(w *gWorld) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := 0
	for _, c := range w.toNode {
		total += c
	}
	return total
}

func send(t *testing.T, n *gNode, ref app.ChatRef, text string) convlog.Message {
	t.Helper()
	m, err := n.app.ChatSend(context.Background(), ref, app.SendReq{Text: text})
	if err != nil {
		t.Fatalf("ChatSend: %v", err)
	}
	return m
}

// ---- 2: saved messages -------------------------------------------------------------------------------------

func TestSavedMessagesAreKeptHereAndNeverSent(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	m := send(t, bob, gref(id, ""), "запомнить")
	w.drain()
	before := totalLetters(w)
	saved, err := alice.app.SaveMessage(app.SavedMessage{ChatID: "g:" + id, ChatName: "Команда", MsgID: m.ID, Sender: bob.nodeHex(), Text: m.Text, TS: m.TS})
	if err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if saved.ID != "g:"+id+"/"+m.ID || saved.SavedAt == 0 {
		t.Fatalf("saved %+v", saved)
	}
	w.clk.off.Add(int64(time.Minute))
	if _, err := alice.app.SaveMessage(app.SavedMessage{ChatID: "g:" + id, MsgID: "second", Text: "ещё"}); err != nil {
		t.Fatal(err)
	}
	list, err := alice.app.ListSavedMessages()
	if err != nil || len(list) != 2 || list[0].MsgID != "second" || list[1].MsgID != m.ID || list[1].Text != "запомнить" {
		t.Fatalf("list %+v, %v", list, err)
	}
	w.drain()
	if got := totalLetters(w); got != before {
		t.Fatalf("saving sent %d letters", got-before)
	}
	// it is this device's: the other runtime has none, and nothing entered the log
	if other, _ := bob.app.ListSavedMessages(); len(other) != 0 {
		t.Fatalf("bob sees %+v", other)
	}
	if page, _ := alice.app.ChatHistory(context.Background(), gref(id, ""), "", 50); len(page.Messages) != 1 {
		t.Fatalf("the log has %d messages", len(page.Messages))
	}
	// saving again updates the copy; unsaving removes it, and a missing one is no error
	if _, err := alice.app.SaveMessage(app.SavedMessage{ChatID: "g:" + id, MsgID: m.ID, Text: "правка"}); err != nil {
		t.Fatal(err)
	}
	list, _ = alice.app.ListSavedMessages()
	if len(list) != 2 || list[0].Text != "правка" {
		t.Fatalf("after an update %+v", list)
	}
	if err := alice.app.UnsaveMessage(list[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := alice.app.UnsaveMessage("g:nothing/none"); err != nil {
		t.Fatalf("unsave of a missing one: %v", err)
	}
	if list, _ = alice.app.ListSavedMessages(); len(list) != 1 {
		t.Fatalf("after unsave %+v", list)
	}
}

func TestSavedMessagesAreBounded(t *testing.T) {
	_, ns := newGroupWorld(t, 1)
	a := ns[0].app
	if _, err := a.SaveMessage(app.SavedMessage{ChatID: "", MsgID: "x"}); !errors.Is(err, app.ErrBadSaved) {
		t.Fatalf("no chat: %v", err)
	}
	if _, err := a.SaveMessage(app.SavedMessage{ChatID: "d:x", MsgID: ""}); !errors.Is(err, app.ErrBadSaved) {
		t.Fatalf("no message: %v", err)
	}
	if _, err := a.SaveMessage(app.SavedMessage{ChatID: strings.Repeat("c", 600), MsgID: "x"}); !errors.Is(err, app.ErrBadSaved) {
		t.Fatalf("long chat id: %v", err)
	}
	// a long text is cut at a rune, not refused
	long := strings.Repeat("я", 40<<10)
	s, err := a.SaveMessage(app.SavedMessage{ChatID: "d:x", MsgID: "long", Text: long})
	if err != nil || len(s.Text) > 64<<10 || !strings.HasPrefix(long, s.Text) {
		t.Fatalf("long text: %d bytes, %v", len(s.Text), err)
	}
	for i := 1; i < app.MaxSaved; i++ {
		if _, err := a.SaveMessage(app.SavedMessage{ChatID: "d:x", MsgID: "m" + strings.Repeat("0", 3) + itoa(i)}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if _, err := a.SaveMessage(app.SavedMessage{ChatID: "d:x", MsgID: "one-too-many"}); !errors.Is(err, app.ErrSavedFull) {
		t.Fatalf("beyond the limit: %v", err)
	}
	// an update of one that is kept is no new bookmark
	if _, err := a.SaveMessage(app.SavedMessage{ChatID: "d:x", MsgID: "long", Text: "ok"}); err != nil {
		t.Fatalf("update at the limit: %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// ---- 3: AddGroupMember -----------------------------------------------------------------------------------

func TestAddGroupMemberSendsAPersonalInvitationAndNothingElse(t *testing.T) {
	w, ns := newGroupWorld(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	alice.befriend(t, bob)
	g, err := alice.app.CreateGroup(ctx, "Команда", "", app.GroupPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.app.GroupAddMember(ctx, g.ID, bob.nodeHex()); err != nil {
		t.Fatalf("GroupAddMember: %v", err)
	}
	w.drain()
	// bob is not in: he must accept
	if ms, _ := alice.app.GroupMembers(g.ID); len(ms) != 1 {
		t.Fatalf("members before the answer: %+v", ms)
	}
	if gs, _ := bob.app.Groups(); len(gs) != 0 {
		t.Fatalf("bob was put into a group: %+v", gs)
	}
	// the invitation is a message in his chat with alice
	page, err := bob.app.ChatHistory(ctx, app.ChatRef{Kind: "dm", ID: alice.nodeHex()}, "", 10)
	if err != nil || len(page.Messages) != 1 || !strings.HasPrefix(page.Messages[0].Text, "echofog-group:2:") {
		t.Fatalf("what bob got: %+v, %v", page.Messages, err)
	}
	// he accepts: only now is he a member
	if _, err := bob.app.JoinGroup(ctx, page.Messages[0].Text); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if r := memberRoles(t, alice, g.ID); r[bob.nodeHex()] != "member" {
		t.Fatalf("after his answer: %v", r)
	}
}

func TestAddGroupMemberRefusals(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	alice.befriend(t, bob)
	if err := alice.app.GroupAddMember(ctx, id, bob.nodeHex()); !errors.Is(err, app.ErrAlreadyMember) {
		t.Fatalf("a member: %v", err)
	}
	if err := alice.app.GroupAddMember(ctx, id, carol.nodeHex()); !errors.Is(err, app.ErrNoContact) {
		t.Fatalf("not a contact: %v", err)
	}
	if err := alice.app.GroupAddMember(ctx, id, "zz"); err == nil {
		t.Fatal("a bad id was taken")
	}
	// a member without the right to make invitations cannot
	bob.befriend(t, ns[0])
	if err := bob.app.GroupAddMember(ctx, id, alice.nodeHex()); err == nil {
		t.Fatal("a member invited without the right")
	}
	// a blocked contact is not invited
	alice.befriend(t, carol)
	if err := alice.app.Block(carol.nodeHex()); err != nil {
		t.Fatal(err)
	}
	if err := alice.app.GroupAddMember(ctx, id, carol.nodeHex()); !errors.Is(err, app.ErrBlocked) {
		t.Fatalf("a blocked contact: %v", err)
	}
	if !strings.Contains(app.UserMessage(app.ErrAlreadyMember), " ") || app.UserMessage(app.ErrAlreadyMember) == app.UserMessage(errors.New("x")) {
		t.Fatal("no user text")
	}
	_ = w
}

// ---- 5: snapshot --------------------------------------------------------------------------------------------

func TestGroupSnapshotCarriesAGroupToANewcomerWithoutAMember(t *testing.T) {
	w, ns := newGroupWorld(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Команда", "чат", app.GroupPrivate)
	inv, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	if _, err := bob.app.JoinGroup(ctx, inv.Link); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if _, err := alice.app.GroupChannelSave(ctx, g.ID, convlog.ChannelSpec{ID: "dev", Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	send(t, alice, gref(g.ID, ""), "первое")
	send(t, bob, gref(g.ID, "dev"), "второе")
	w.drain()
	path := filepath.Join(t.TempDir(), "team.fogzip")
	if err := alice.app.GroupSnapshotExport(ctx, g.ID, path, "correct horse"); err != nil {
		t.Fatalf("export: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(raw), "FOGZIP2\n") {
		t.Fatalf("file: %v %q", err, raw[:min(8, len(raw))])
	}
	if strings.Contains(string(raw), "первое") {
		t.Fatal("the text is not sealed")
	}
	// the newcomer opens it with no member reachable: nothing is delivered between the import and the read
	info, n, err := carol.app.GroupSnapshotImport(ctx, path, "correct horse")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if info.ID != g.ID || info.Title != "Команда" || n < 5 {
		t.Fatalf("imported %+v, %d events", info, n)
	}
	page, err := carol.app.ChatHistory(ctx, gref(g.ID, ""), "", 20)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Text != "первое" {
		t.Fatalf("the main channel before any delivery: %+v, %v", page.Messages, err)
	}
	if page, _ := carol.app.ChatHistory(ctx, gref(g.ID, "dev"), "", 20); len(page.Messages) != 1 || page.Messages[0].Text != "второе" {
		t.Fatalf("the channel: %+v", page.Messages)
	}
	if chs, _ := carol.app.GroupChannels(g.ID); len(chs) != 1 || chs[0].ID != "dev" {
		t.Fatalf("channels %+v", chs)
	}
	w.drain()
	if r := memberRoles(t, alice, g.ID); r[carol.nodeHex()] != "member" {
		t.Fatalf("members after the join: %v", r)
	}
}

func TestGroupSnapshotRefusals(t *testing.T) {
	w, ns := newGroupWorld(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Команда", "", app.GroupPrivate)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.fogzip")
	if err := alice.app.GroupSnapshotExport(ctx, g.ID, path, "short"); !errors.Is(err, app.ErrSnapshotPassword) {
		t.Fatalf("a short password: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a file was written")
	}
	if err := alice.app.GroupSnapshotExport(ctx, "nothing", path, "long enough pass"); err == nil {
		t.Fatal("an unknown group")
	}
	if err := alice.app.GroupSnapshotExport(ctx, g.ID, path, "long enough pass"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bob.app.GroupSnapshotImport(ctx, path, "another password"); !errors.Is(err, app.ErrSnapshotFile) {
		t.Fatalf("a wrong password: %v", err)
	}
	// the file of v1 is not taken, and says so
	v1 := filepath.Join(dir, "v1.fogzip")
	_ = os.WriteFile(v1, append([]byte("FOGZIP1\n"), make([]byte, 100)...), 0o600)
	if _, _, err := bob.app.GroupSnapshotImport(ctx, v1, "long enough pass"); !errors.Is(err, app.ErrSnapshotVersion) {
		t.Fatalf("a file of v1: %v", err)
	}
	junk := filepath.Join(dir, "junk.fogzip")
	_ = os.WriteFile(junk, []byte("hello"), 0o600)
	if _, _, err := bob.app.GroupSnapshotImport(ctx, junk, "long enough pass"); !errors.Is(err, app.ErrSnapshotFile) {
		t.Fatalf("junk: %v", err)
	}
	if _, _, err := bob.app.GroupSnapshotImport(ctx, filepath.Join(dir, "none"), "long enough pass"); err == nil {
		t.Fatal("a missing file")
	}
	// nothing of the refused files is left in bob's groups
	w.drain()
	if gs, _ := bob.app.Groups(); len(gs) != 0 {
		t.Fatalf("groups after refusals: %+v", gs)
	}
	for _, e := range []error{app.ErrSnapshotPassword, app.ErrSnapshotFile, app.ErrSnapshotVersion} {
		if app.UserMessage(e) == app.UserMessage(errors.New("x")) {
			t.Fatalf("no user text for %v", e)
		}
	}
	if app.UserMessage(app.ErrSnapshotVersion) == app.UserMessage(app.ErrSnapshotFile) {
		t.Fatal("the version screen has its own text")
	}
}

// every event of a snapshot is weighed as one sent by a member: a changed one and one of a stranger are dropped.
func TestGroupSnapshotEventsAreVerifiedOnImport(t *testing.T) {
	w, ns := newGroupWorld(t, 4)
	alice, bob, carol, dave := ns[0], ns[1], ns[2], ns[3]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Команда", "", app.GroupPrivate)
	inv, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	_, _ = bob.app.JoinGroup(ctx, inv.Link)
	w.drain()
	send(t, alice, gref(g.ID, ""), "настоящее")
	send(t, bob, gref(g.ID, ""), "подлинное")
	w.drain()
	path := filepath.Join(t.TempDir(), "t.fogzip")
	if err := alice.app.GroupSnapshotExport(ctx, g.ID, path, "long enough pass"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	doc, err := app.OpenSnapshotForTest(raw, "long enough pass")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, honest, err := carol.app.GroupSnapshotImport(ctx, path, "long enough pass")
	if err != nil {
		t.Fatal(err)
	}
	// a copy with one event changed in a byte: that event is dropped, the others stay
	forged := *doc
	forged.Events = append([][]byte(nil), doc.Events...)
	last := len(forged.Events) - 1
	ev := append([]byte(nil), forged.Events[last]...)
	ev[len(ev)-3] ^= 0x01
	forged.Events[last] = ev
	// an event nobody in the group wrote: bob's event under a conversation of a group that is not this one
	sealed, err := app.SealSnapshotForTest(&forged, "long enough pass")
	if err != nil {
		t.Fatal(err)
	}
	fpath := filepath.Join(t.TempDir(), "f.fogzip")
	_ = os.WriteFile(fpath, sealed, 0o600)
	_, bad, err := dave.app.GroupSnapshotImport(ctx, fpath, "long enough pass")
	if err != nil {
		t.Fatalf("import of the changed file: %v", err)
	}
	if bad >= honest {
		t.Fatalf("a changed event was taken: %d events against %d", bad, honest)
	}
	page, _ := dave.app.ChatHistory(ctx, gref(g.ID, ""), "", 20)
	for _, m := range page.Messages {
		if m.Text != "настоящее" && m.Text != "подлинное" {
			t.Fatalf("a message nobody wrote: %q", m.Text)
		}
	}
	// an invitation that is not signed by its maker is refused as a whole
	forged.Invite = forged.Invite[:len(forged.Invite)-4] + "AAAA"
	sealed, _ = app.SealSnapshotForTest(&forged, "long enough pass")
	_ = os.WriteFile(fpath, sealed, 0o600)
	w2, ns2 := newGroupWorld(t, 1)
	_ = w2
	if _, _, err := ns2[0].app.GroupSnapshotImport(ctx, fpath, "long enough pass"); !errors.Is(err, app.ErrInvite) {
		t.Fatalf("a forged invitation: %v", err)
	}
	if gs, _ := ns2[0].app.Groups(); len(gs) != 0 {
		t.Fatalf("a forged file made a group: %+v", gs)
	}
}

// ---- 6: MutedUntil ---------------------------------------------------------------------------------------------

func TestMutedUntilSilencesAChatForAWhileOnThisDevice(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	ref := gref(id, "")
	now := w.clk.Now().Unix()
	if err := alice.app.ChatMuteUntil(ref, now+3600); err != nil {
		t.Fatalf("mute: %v", err)
	}
	if got, err := alice.app.ChatMutedUntil(ref); err != nil || got != now+3600 {
		t.Fatalf("muted until %d, %v", got, err)
	}
	if got, _ := bob.app.ChatMutedUntil(ref); got != 0 {
		t.Fatalf("bob is muted too: %d", got)
	}
	m := send(t, bob, ref, "тихо")
	w.drain()
	got, _ := alice.app.ChatMessage(ctx, ref, m.ID)
	if a := alice.app.ChatAlert(got); a != app.AlertNone {
		t.Fatalf("a muted chat alerts: %s", a)
	}
	// being named still counts, as with the mode "nothing"
	named, _ := bob.app.ChatSend(ctx, ref, app.SendReq{Text: "эй", Mentions: []string{alice.nodeHex()}})
	w.drain()
	got, _ = alice.app.ChatMessage(ctx, ref, named.ID)
	if a := alice.app.ChatAlert(got); a != app.AlertLoud {
		t.Fatalf("being named while muted: %s", a)
	}
	chats, _ := alice.app.Conversations()
	if len(chats) != 1 || chats[0].MutedUntil != now+3600 {
		t.Fatalf("the list does not show it: %+v", chats)
	}
	// it runs out by itself
	w.clk.off.Add(int64(2 * time.Hour))
	if got, _ := alice.app.ChatMutedUntil(ref); got != 0 {
		t.Fatalf("still muted after the term: %d", got)
	}
	if a := alice.app.ChatAlert(m2(alice, ref, m.ID)); a != app.AlertQuiet {
		t.Fatalf("after the term: %s", a)
	}
	// clear, a past time clears, too far is refused
	_ = alice.app.ChatMuteUntil(ref, w.clk.Now().Unix()+60)
	if err := alice.app.ChatMuteUntil(ref, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := alice.app.ChatMutedUntil(ref); got != 0 {
		t.Fatalf("not cleared: %d", got)
	}
	_ = alice.app.ChatMuteUntil(ref, w.clk.Now().Unix()+60)
	if err := alice.app.ChatMuteUntil(ref, w.clk.Now().Unix()-5); err != nil {
		t.Fatal(err)
	}
	if got, _ := alice.app.ChatMutedUntil(ref); got != 0 {
		t.Fatalf("a past time did not clear: %d", got)
	}
	if err := alice.app.ChatMuteUntil(ref, w.clk.Now().Add(400*24*time.Hour).Unix()); !errors.Is(err, app.ErrRequest) {
		t.Fatalf("too long: %v", err)
	}
	if err := alice.app.ChatMuteUntil(gref("nothing", ""), now+60); !errors.Is(err, app.ErrNoChat) {
		t.Fatalf("an unknown chat: %v", err)
	}
}

func m2(n *gNode, ref app.ChatRef, id string) convlog.Message {
	m, _ := n.app.ChatMessage(context.Background(), ref, id)
	return m
}

func TestMutedUntilSendsNothing(t *testing.T) {
	w, ns, id := team(t, 2)
	w.drain()
	before := totalLetters(w)
	_ = ns[0].app.ChatMuteUntil(gref(id, ""), w.clk.Now().Unix()+600)
	w.drain()
	if totalLetters(w) != before {
		t.Fatal("muting sent letters")
	}
}

// ---- 7: unread and the last message of each channel ----------------------------------------------------------

func TestChannelStatsCarryUnreadAndTheLastMessage(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	if _, err := alice.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: "dev", Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: "quiet", Name: "quiet"}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	send(t, bob, gref(id, "dev"), "раз")
	send(t, bob, gref(id, "dev"), "два")
	last := send(t, bob, gref(id, "dev"), "три")
	send(t, bob, gref(id, ""), "в главном")
	if _, err := bob.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "тебе", Mentions: []string{alice.nodeHex()}}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	st, err := alice.app.GroupChannelStats(id)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	dev := st["dev"]
	if dev.Unread != 3 || dev.Last == nil || dev.Last.Text != "три" || dev.LastTS == 0 {
		t.Fatalf("dev: %+v last=%+v", dev, dev.Last)
	}
	if main := st[""]; main.Unread != 2 || main.Mentions != 1 || main.Last == nil || main.Last.Text != "тебе" {
		t.Fatalf("main: %+v", main)
	}
	if q, ok := st["quiet"]; ok && (q.Unread != 0 || q.Last != nil) {
		t.Fatalf("a channel with no news: %+v", q)
	}
	// reading is the reader's own mark: the count drops, the last message stays
	if err := alice.app.ChatMarkRead(ctx, gref(id, "dev"), last.ID); err != nil {
		t.Fatal(err)
	}
	st, _ = alice.app.GroupChannelStats(id)
	if st["dev"].Unread != 0 || st["dev"].Last == nil || st["dev"].Last.Text != "три" {
		t.Fatalf("after reading: %+v", st["dev"])
	}
	if st[""].Unread != 2 {
		t.Fatalf("reading dev touched the main channel: %+v", st[""])
	}
	// writing in a channel is reading it: the own message is the last one and nothing is unread
	own := send(t, alice, gref(id, "dev"), "мой ответ")
	if st, _ = alice.app.GroupChannelStats(id); st["dev"].Unread != 0 || st["dev"].Last == nil || st["dev"].Last.ID != own.ID {
		t.Fatalf("after an own message: %+v", st["dev"])
	}
	if _, err := alice.app.GroupChannelStats("nothing"); err == nil {
		t.Fatal("an unknown group")
	}
}

// ---- 8: the web link of an invitation ----------------------------------------------------------------------

func withJoinHost(host string) func() {
	groupWorldConfig = func(c *app.Config) { c.JoinHost = host }
	return func() { groupWorldConfig = nil }
}

func TestGroupInviteHasAWebLinkWithTheDataInTheFragment(t *testing.T) {
	defer withJoinHost("join.example.org")()
	w, ns := newGroupWorld(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Команда", "", app.GroupPrivate)
	inv, err := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://join.example.org/join/#group/" + strings.TrimPrefix(inv.Link, "echofog-group:2:")
	if inv.Web != want {
		t.Fatalf("web link %q, want %q", inv.Web, want)
	}
	u, err := url.Parse(inv.Web)
	if err != nil || u.RawQuery != "" || u.User != nil || !strings.HasPrefix(u.Fragment, "group/") || strings.Contains(u.Path, "group") {
		t.Fatalf("the data is not in the fragment alone: %+v %v", u, err)
	}
	if list, _ := alice.app.GroupInvites(g.ID); len(list) != 1 || list[0].Web != want {
		t.Fatalf("the list lacks it: %+v", list)
	}
	// the page's form opens like the app's
	pv, err := bob.app.InspectGroupInvite(inv.Web)
	if err != nil || pv.GroupID != g.ID || pv.Title != "Команда" {
		t.Fatalf("inspect by the web link: %+v, %v", pv, err)
	}
	if _, err := bob.app.JoinGroup(ctx, inv.Web); err != nil {
		t.Fatalf("join by the web link: %v", err)
	}
	w.drain()
	if r := memberRoles(t, alice, g.ID); r[bob.nodeHex()] != "member" {
		t.Fatalf("members: %v", r)
	}
	// the data in a query, or after a user part, is not a link
	bad := []string{
		"https://join.example.org/join/?group/" + strings.TrimPrefix(inv.Link, "echofog-group:2:"),
		"https://join.example.org/join/?x=1#group/" + strings.TrimPrefix(inv.Link, "echofog-group:2:"),
		"https://u@join.example.org/join/#group/" + strings.TrimPrefix(inv.Link, "echofog-group:2:"),
		"https://join.example.org/join/#group/",
		"https://join.example.org/join/#contact/" + strings.TrimPrefix(inv.Link, "echofog-group:2:"),
	}
	for _, l := range bad {
		if _, err := bob.app.InspectGroupInvite(l); !errors.Is(err, app.ErrInvite) {
			t.Errorf("%q: %v", l, err)
		}
	}
}

func TestGroupInviteHasNoWebLinkWithoutAJoinPage(t *testing.T) {
	_, ns := newGroupWorld(t, 1)
	g, _ := ns[0].app.CreateGroup(context.Background(), "Команда", "", app.GroupPrivate)
	inv, err := ns[0].app.GroupInviteCreate(context.Background(), g.ID, app.InviteSpec{TTL: time.Hour})
	if err != nil || inv.Web != "" || inv.Link == "" {
		t.Fatalf("%+v, %v", inv, err)
	}
}

// ---- 9: small things of the acceptance -----------------------------------------------------------------------

func chatChanges(a *app.App) (*[]app.ConvChange, func()) {
	var got []app.ConvChange
	cancel := a.SubscribeChats(func(c app.ConvChange) { got = append(got, c) })
	return &got, cancel
}

func TestOnlyNewMessagesAreFreshNotEditsReactionsOrPins(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	ref := gref(id, "")
	got, stop := chatChanges(alice.app)
	defer stop()
	root := send(t, bob, ref, "корень")
	w.drain()
	fresh := func() []string {
		var out []string
		for _, c := range *got {
			out = append(out, c.Fresh...)
		}
		return out
	}
	if f := fresh(); len(f) != 1 || f[0] != root.ID {
		t.Fatalf("a new message: %v", f)
	}
	unread := func() int {
		cs, _ := alice.app.Conversations()
		return cs[0].Unread
	}
	if unread() != 1 {
		t.Fatalf("unread %d", unread())
	}
	*got = nil
	if err := bob.app.ChatEdit(ctx, ref, root.ID, "правка"); err != nil {
		t.Fatal(err)
	}
	if err := bob.app.ChatReact(ctx, ref, root.ID, "👍", true); err != nil {
		t.Fatal(err)
	}
	if err := alice.app.ChatPin(ctx, ref, root.ID, true); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if len(*got) == 0 {
		t.Fatal("the edit made no change for conv:change")
	}
	if f := fresh(); len(f) != 0 {
		t.Fatalf("an edit, a reaction or a pin counted as new: %v", f)
	}
	if unread() != 1 {
		t.Fatalf("unread grew to %d", unread())
	}
	// a reply is new; the root it answers is not
	*got = nil
	reply, err := bob.app.ChatSend(ctx, ref, app.SendReq{Text: "ответ", Thread: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	w.drain()
	if f := fresh(); len(f) != 1 || f[0] != reply.ID {
		t.Fatalf("a reply: %v", f)
	}
	// an old message that is reacted to after a restart of the subscription is still not new
	stop()
	got2, stop2 := chatChanges(alice.app)
	defer stop2()
	if err := bob.app.ChatReact(ctx, ref, root.ID, "❤", true); err != nil {
		t.Fatal(err)
	}
	w.drain()
	for _, c := range *got2 {
		if len(c.Fresh) != 0 {
			t.Fatalf("a reaction on an old message after a new subscription: %v", c.Fresh)
		}
	}
}

func TestLeavingAGroupGivesNoRemovedEventToTheLeaver(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	var bobEv, carolEv []app.GroupEvent
	cb := bob.app.SubscribeGroupEvents(func(e app.GroupEvent) { bobEv = append(bobEv, e) })
	defer cb()
	cc := carol.app.SubscribeGroupEvents(func(e app.GroupEvent) { carolEv = append(carolEv, e) })
	defer cc()
	if err := bob.app.LeaveGroup(ctx, id); err != nil {
		t.Fatal(err)
	}
	w.drain()
	for _, e := range bobEv {
		if e.Kind == "removed" {
			t.Fatalf("the one who left was told they were removed: %+v", e)
		}
	}
	// being removed by somebody else is still told
	if err := alice.app.GroupRemove(ctx, id, carol.nodeHex(), false); err != nil {
		t.Fatal(err)
	}
	w.drain()
	var told bool
	for _, e := range carolEv {
		if e.Kind == "removed" && e.Reason == "removed" {
			told = true
		}
	}
	if !told {
		t.Fatalf("the removed member was not told: %+v", carolEv)
	}
}
