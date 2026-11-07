package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
)

// ---- calls of groups and of voice rooms (T11.4) --------------------------------------------------------------

func callEvents(n *gNode) (*evLog, func()) {
	var ev evLog
	return &ev, n.app.SubscribeGroupEvents(ev.add)
}

func TestGroupCallIsAnnouncedAndRings(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	evB, stopB := callEvents(bob)
	defer stopB()
	evC, stopC := callEvents(carol)
	defer stopC()
	evA, stopA := callEvents(alice)
	defer stopA()

	c, err := alice.app.StartGroupCall(context.Background(), id)
	if err != nil {
		t.Fatalf("StartGroupCall: %v", err)
	}
	if !strings.HasPrefix(c.CallID, "call_") || c.Coordinator == "" || c.Room != "" || c.StartedAt == 0 {
		t.Fatalf("call %+v", c)
	}
	w.drain()
	for name, ev := range map[string]*evLog{"bob": evB, "carol": evC} {
		got := ev.of("call")
		if len(got) != 1 || got[0].Group != id || got[0].CallID != c.CallID || got[0].Coordinator != c.Coordinator || got[0].From != alice.nodeHex() || got[0].Room != "" {
			t.Fatalf("%s: events %+v", name, ev.got)
		}
	}
	if len(evA.of("call")) != 0 {
		t.Fatalf("the caller was rung: %+v", evA.got)
	}
	for _, n := range ns {
		got, ok := n.app.ActiveGroupCall(id, "")
		if !ok || got.CallID != c.CallID || got.Coordinator != c.Coordinator {
			t.Fatalf("a member sees the call as %+v, %v", got, ok)
		}
	}
	// a second start in the group joins the call that is going on
	again, err := bob.app.StartGroupCall(context.Background(), id)
	if err != nil || again.CallID != c.CallID {
		t.Fatalf("a second start made %+v, %v", again, err)
	}
}

func TestGroupCallKeepAliveTellsTheOnesWhoMissedTheStart(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	c, _ := alice.app.StartGroupCall(ctx, id)
	w.drain()
	// carol was offline for the start: she forgets it (a fresh app would know nothing)
	evC, stop := callEvents(carol)
	defer stop()
	carol.app.ForgetGroupCalls()
	if err := alice.app.KeepGroupCallAlive(ctx, id, "", c.CallID, c.Coordinator); err != nil {
		t.Fatal(err)
	}
	w.drain()
	got := evC.of("callactive")
	if len(got) != 1 || got[0].CallID != c.CallID || got[0].Coordinator != c.Coordinator {
		t.Fatalf("a call going on that was not heard of: %+v", evC.got)
	}
	if len(evC.of("call")) != 0 {
		t.Fatal("a call under way rang")
	}
	// more of the same keep-alive changes nothing
	w.clk.off.Add(int64(50 * time.Second))
	_ = alice.app.KeepGroupCallAlive(ctx, id, "", c.CallID, c.Coordinator)
	w.drain()
	if len(evC.of("callactive")) != 1 {
		t.Fatalf("callactive again: %+v", evC.got)
	}
	// bob joins: the call lists him among who is in it
	if err := bob.app.KeepGroupCallAlive(ctx, id, "", c.CallID, c.Coordinator); err != nil {
		t.Fatal(err)
	}
	w.drain()
	got2, _ := alice.app.ActiveGroupCall(id, "")
	if len(got2.Members) != 2 {
		t.Fatalf("members of the call: %v", got2.Members)
	}
}

func TestGroupCallEndsAndExpires(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	evB, stop := callEvents(bob)
	defer stop()
	c, _ := alice.app.StartGroupCall(ctx, id)
	w.drain()
	if err := alice.app.EndGroupCall(ctx, id, "", c.CallID); err != nil {
		t.Fatalf("EndGroupCall: %v", err)
	}
	w.drain()
	end := evB.of("callend")
	if len(end) != 1 || end[0].CallID != c.CallID || end[0].Group != id {
		t.Fatalf("end events %+v", evB.got)
	}
	for _, n := range ns {
		if _, ok := n.app.ActiveGroupCall(id, ""); ok {
			t.Fatal("an ended call is still active")
		}
	}
	if err := alice.app.EndGroupCall(ctx, id, "", c.CallID); err == nil {
		t.Fatal("ending a call that is over succeeded")
	}
	// a call nobody keeps alive is over by itself
	c2, _ := alice.app.StartGroupCall(ctx, id)
	w.drain()
	w.clk.off.Add(int64(3 * time.Minute))
	if _, ok := bob.app.ActiveGroupCall(id, ""); ok {
		t.Fatalf("a call nobody kept alive is still active: %s", c2.CallID)
	}
}

func TestVoiceRoomCallNeverRingsAndHasItsOwnState(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	if _, err := alice.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: "room", Name: "Комната", Voice: true}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	evB, stop := callEvents(bob)
	defer stop()
	c, err := alice.app.StartRoomCall(ctx, id, "room")
	if err != nil {
		t.Fatalf("StartRoomCall: %v", err)
	}
	w.drain()
	if len(evB.of("call")) != 0 {
		t.Fatalf("a voice room rang: %+v", evB.got)
	}
	got := evB.of("callactive")
	if len(got) != 1 || got[0].Room != "room" || got[0].CallID != c.CallID {
		t.Fatalf("room events %+v", evB.got)
	}
	if _, ok := bob.app.ActiveGroupCall(id, ""); ok {
		t.Fatal("a room's call is the group's call")
	}
	if rc, ok := bob.app.ActiveGroupCall(id, "room"); !ok || rc.CallID != c.CallID || rc.Room != "room" {
		t.Fatalf("room call %+v %v", rc, ok)
	}
	// who stays: leaving takes the member out of the list, the call goes on
	_ = bob.app.KeepGroupCallAlive(ctx, id, "room", c.CallID, c.Coordinator)
	w.drain()
	if rc, _ := alice.app.ActiveGroupCall(id, "room"); len(rc.Members) != 2 {
		t.Fatalf("members %v", rc.Members)
	}
	if err := bob.app.LeaveRoomCall(ctx, id, "room", c.CallID); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if rc, ok := alice.app.ActiveGroupCall(id, "room"); !ok || len(rc.Members) != 1 {
		t.Fatalf("after leaving: %+v %v", rc, ok)
	}
	if err := alice.app.EndGroupCall(ctx, id, "room", c.CallID); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if len(evB.of("callend")) != 1 || evB.of("callend")[0].Room != "room" {
		t.Fatalf("room end %+v", evB.got)
	}
	// a room that is not a voice room, or not there
	if _, err := alice.app.StartRoomCall(ctx, id, "nope"); err == nil {
		t.Fatal("a call in a room that is not there")
	}
}

func TestGroupCallRefusals(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	// a stranger and an unknown group
	if _, err := alice.app.StartGroupCall(ctx, "no-such-group"); !errors.Is(err, app.ErrNoChat) && !errors.Is(err, app.ErrNoGroup) {
		t.Fatalf("unknown group: %v", err)
	}
	// a member whose role may not call: a read-only role
	if _, err := alice.app.GroupRoleSave(ctx, id, app.GroupRole{ID: "listener", Title: "Слушатель", Rank: 5, Perms: convlog.PermSend}); err != nil {
		t.Fatal(err)
	}
	if err := alice.app.GroupSetRole(ctx, id, bob.nodeHex(), "listener"); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if _, err := bob.app.StartGroupCall(ctx, id); !errors.Is(err, app.ErrNoRight) {
		t.Fatalf("a role without the right to call: %v", err)
	}
	// a removed member's announcement is not heard
	if err := alice.app.GroupRemove(ctx, id, carol.nodeHex(), false); err != nil {
		t.Fatal(err)
	}
	w.drain()
	evA, stop := callEvents(alice)
	defer stop()
	sendCallWire(t, w, carol, alice, `{"v":1,"k":"call","g":"`+id+`","c":"call_forged","o":"`+carol.nodeHex()+`"}`)
	w.drain()
	if len(evA.got) != 0 {
		t.Fatalf("a removed member rang the group: %+v", evA.got)
	}
	if _, ok := alice.app.ActiveGroupCall(id, ""); ok {
		t.Fatal("a removed member's call is active")
	}
}

// sendCallWire hands alice the announcement as a chat/2 message of one account, whatever it says.
func sendCallWire(t *testing.T, w *gWorld, from, to *gNode, payload string) {
	t.Helper()
	w.mu.Lock()
	w.queue = append(w.queue, gLetter{from: from.node, to: to.node, pm: &chatpb.PersonalMessageProto{Metadata: "fogcall2:" + payload}})
	w.mu.Unlock()
}

func TestGroupCallAnnouncementsAreHardenedAgainstJunk(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	evA, stop := callEvents(alice)
	defer stop()
	for name, payload := range map[string]string{
		"not json":         `{{{`,
		"version":          `{"v":2,"k":"call","g":"` + id + `","c":"call_x","o":"` + bob.nodeHex() + `"}`,
		"unknown kind":     `{"v":1,"k":"boom","g":"` + id + `","c":"call_x","o":"` + bob.nodeHex() + `"}`,
		"unknown group":    `{"v":1,"k":"call","g":"nope","c":"call_x","o":"` + bob.nodeHex() + `"}`,
		"coordinator none": `{"v":1,"k":"call","g":"` + id + `","c":"call_x","o":"` + strings.Repeat("0", 64) + `"}`,
		"no call id":       `{"v":1,"k":"call","g":"` + id + `","c":"","o":"` + bob.nodeHex() + `"}`,
		"long call id":     `{"v":1,"k":"call","g":"` + id + `","c":"` + strings.Repeat("a", 400) + `","o":"` + bob.nodeHex() + `"}`,
		"unknown room":     `{"v":1,"k":"call","g":"` + id + `","r":"nope","c":"call_x","o":"` + bob.nodeHex() + `"}`,
		"huge":             `{"v":1,"k":"call","g":"` + id + `","c":"call_x","o":"` + bob.nodeHex() + `","x":"` + strings.Repeat("a", 5000) + `"}`,
	} {
		sendCallWire(t, w, bob, alice, payload)
		w.drain()
		if len(evA.got) != 0 {
			t.Fatalf("%s: an event %+v", name, evA.got)
		}
		if _, ok := alice.app.ActiveGroupCall(id, ""); ok {
			t.Fatalf("%s: a call became active", name)
		}
	}
	// the end of a call nobody knows is silence
	sendCallWire(t, w, bob, alice, `{"v":1,"k":"end","g":"`+id+`","c":"call_unknown"}`)
	w.drain()
	if len(evA.got) != 0 {
		t.Fatalf("an unknown end: %+v", evA.got)
	}
	// the announcement is not a chat message
	if got := historyLen(t, alice, gref(id, "")); got != 0 {
		t.Fatalf("an announcement was shown as a message: %d", got)
	}
}

func TestBlockedContactsCallAnnouncementIsDropped(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	alice.befriend(t, bob)
	if err := alice.app.Block(bob.nodeHex()); err != nil {
		t.Fatal(err)
	}
	evA, stop := callEvents(alice)
	defer stop()
	if _, err := bob.app.StartGroupCall(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if len(evA.got) != 0 {
		t.Fatalf("a blocked contact rang: %+v", evA.got)
	}
}
