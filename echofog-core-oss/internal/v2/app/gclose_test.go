package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
)

func groupEvents(n *gNode) (*[]app.GroupEvent, func()) {
	var got []app.GroupEvent
	cancel := n.app.SubscribeGroupEvents(func(e app.GroupEvent) { got = append(got, e) })
	return &got, cancel
}

func TestTheOwnerClosesAGroupForEverybody(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	send(t, bob, gref(id, ""), "до закрытия")
	w.drain()
	inv, _ := alice.app.GroupInviteCreate(ctx, id, app.InviteSpec{TTL: time.Hour}) // made while it was open
	w.drain()
	bobEv, stopB := groupEvents(bob)
	defer stopB()
	aliceEv, stopA := groupEvents(alice)
	defer stopA()

	// a member cannot close it, nothing changes
	if err := bob.app.GroupClose(ctx, id); !errors.Is(err, app.ErrNoRight) {
		t.Fatalf("a member closes: %v", err)
	}
	if err := alice.app.GroupClose(ctx, id); err != nil {
		t.Fatalf("GroupClose: %v", err)
	}
	w.drain()
	for name, n := range map[string]*gNode{"alice": alice, "bob": bob, "carol": carol} {
		g, err := n.app.GroupInfoOf(id)
		if err != nil || !g.Closed {
			t.Fatalf("%s: %+v %v", name, g, err)
		}
		// the archive is there to read, and nobody may write
		if page, err := n.app.ChatHistory(ctx, gref(id, ""), "", 10); err != nil || len(page.Messages) != 1 || page.Messages[0].Text != "до закрытия" {
			t.Fatalf("%s: archive %+v %v", name, page.Messages, err)
		}
		if r, err := n.app.GroupRights(id, ""); err != nil || r.Send || r.Media || r.Invite || !r.Member {
			t.Fatalf("%s: rights %+v %v", name, r, err)
		}
		if _, err := n.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "ещё"}); !errors.Is(err, app.ErrGroupClosed) {
			t.Fatalf("%s: a write to a closed group: %v", name, err)
		}
	}
	// the members are told, the owner who closed it is not
	var told bool
	for _, e := range *bobEv {
		if e.Kind == "removed" && e.Reason == "deleted" && e.Group == id {
			told = true
		}
	}
	if !told {
		t.Fatalf("bob was not told: %+v", *bobEv)
	}
	for _, e := range *aliceEv {
		if e.Kind == "removed" {
			t.Fatalf("the owner was told they were removed: %+v", e)
		}
	}
	// new entries are refused, without a word to the newcomer
	wd, nd := newGroupWorld(t, 1)
	_ = wd
	dave := nd[0]
	w.pubs[dave.node] = dave.pub
	w.nodes[dave.node] = dave
	dave.world = w
	if _, err := dave.app.JoinGroup(ctx, inv.Link); err != nil {
		t.Fatalf("the newcomer is told: %v", err)
	}
	w.drain()
	if ms, _ := alice.app.GroupMembers(id); len(ms) != 3 {
		t.Fatalf("a newcomer joined a closed group: %+v", ms)
	}
	if _, err := alice.app.GroupInviteCreate(ctx, id, app.InviteSpec{TTL: time.Hour}); err == nil {
		t.Fatal("an invitation was made in a closed group")
	}
	// the same call again changes nothing
	if err := alice.app.GroupClose(ctx, id); err != nil {
		t.Fatalf("a second close: %v", err)
	}
	if app.UserMessage(app.ErrGroupClosed) == app.UserMessage(errors.New("x")) {
		t.Fatal("no user text for a closed group")
	}
	// "delete for me" is local: bob's copy goes, the others keep theirs
	if err := bob.app.ForgetGroup(id); err != nil {
		t.Fatalf("delete for me: %v", err)
	}
	if gs, _ := bob.app.Groups(); len(gs) != 0 {
		t.Fatalf("bob still has it: %+v", gs)
	}
	if g, err := carol.app.GroupInfoOf(id); err != nil || !g.Closed {
		t.Fatalf("carol lost her archive: %+v %v", g, err)
	}
	// leaving a closed group is the same local act
	if err := carol.app.LeaveGroup(ctx, id); err != nil {
		t.Fatalf("leave a closed group: %v", err)
	}
	if g, err := alice.app.GroupInfoOf(id); err != nil || !g.Closed {
		t.Fatalf("alice: %+v %v", g, err)
	}
}

func TestAMessageWrittenBeforeTheCloseArrivedIsNotKept(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	send(t, bob, gref(id, ""), "до")
	w.drain()
	if err := alice.app.GroupClose(ctx, id); err != nil {
		t.Fatal(err)
	}
	// carol has not heard yet and writes on; her message is beyond the vector the owner named
	send(t, carol, gref(id, ""), "поздно")
	w.drain()
	for name, n := range map[string]*gNode{"alice": alice, "bob": bob, "carol": carol} {
		page, err := n.app.ChatHistory(ctx, gref(id, ""), "", 20)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, m := range page.Messages {
			if m.Text == "поздно" {
				t.Fatalf("%s keeps a message beyond the close vector", name)
			}
		}
	}
}

// ---- the group file (.fogzip) carries the file cards and the close ---------------------------------------------

func TestSnapshotCarriesTheFileCardOfAGroup(t *testing.T) {
	w, ns := newGroupWorld(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Команда", "", app.GroupPrivate)
	inv, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	if _, err := bob.app.JoinGroup(ctx, inv.Link); err != nil {
		t.Fatal(err)
	}
	w.drain()
	path, data := writeTemp(t, 700)
	tr, err := alice.app.SendGroupFile(ctx, g.ID, "", "", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = alice.app.CancelFile(tr.ID) })
	w.drain()
	snap := filepath.Join(t.TempDir(), "t.fogzip")
	if err := alice.app.GroupSnapshotExport(ctx, g.ID, snap, "long enough pass"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := carol.app.GroupSnapshotImport(ctx, snap, "long enough pass"); err != nil {
		t.Fatal(err)
	}
	w.drain()
	page, err := carol.app.ChatHistory(ctx, gref(g.ID, ""), "", 20)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].File == nil || page.Messages[0].File.ID != tr.ID {
		t.Fatalf("the card is not in the imported group: %+v %v", page.Messages, err)
	}
	got := accepted(t, carol, tr.ID)
	if b, _ := os.ReadFile(got.Path); string(b) != string(data) {
		t.Fatal("the bytes differ")
	}
}

func TestSnapshotOfAClosedGroupCarriesTheCloseAndLetsNobodyIn(t *testing.T) {
	w, ns := newGroupWorld(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Команда", "", app.GroupPrivate)
	inv, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	if _, err := bob.app.JoinGroup(ctx, inv.Link); err != nil {
		t.Fatal(err)
	}
	w.drain()
	send(t, bob, gref(g.ID, ""), "было")
	w.drain()
	snap := filepath.Join(t.TempDir(), "open.fogzip")
	if err := alice.app.GroupSnapshotExport(ctx, g.ID, snap, "long enough pass"); err != nil {
		t.Fatal(err)
	}
	if err := alice.app.GroupClose(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	w.drain()
	// a closed group makes no new snapshot (its invitations are closed with it)
	if err := alice.app.GroupSnapshotExport(ctx, g.ID, filepath.Join(t.TempDir(), "x.fogzip"), "long enough pass"); !errors.Is(err, app.ErrGroupClosed) {
		t.Fatalf("a snapshot of a closed group: %v", err)
	}
	// the old file now carries the events up to the close, as a member of the group had them
	raw, _ := os.ReadFile(snap)
	doc, err := app.OpenSnapshotForTest(raw, "long enough pass")
	if err != nil {
		t.Fatal(err)
	}
	doc.Events = alice.eng.Log().Export(convlog.Group(g.ID), 1<<20)
	sealed, _ := app.SealSnapshotForTest(doc, "long enough pass")
	late := filepath.Join(t.TempDir(), "closed.fogzip")
	_ = os.WriteFile(late, sealed, 0o600)
	if _, _, err := carol.app.GroupSnapshotImport(ctx, late, "long enough pass"); err != nil {
		t.Fatalf("import: %v", err)
	}
	w.drain()
	info, err := carol.app.GroupInfoOf(g.ID)
	if err != nil || !info.Closed {
		t.Fatalf("the close did not arrive with the file: %+v %v", info, err)
	}
	if ms, _ := alice.app.GroupMembers(g.ID); len(ms) != 2 {
		t.Fatalf("the importer joined a closed group: %+v", ms)
	}
}
