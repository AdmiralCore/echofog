package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// ---- T11.4 acceptance: M1, M2, L1 -------------------------------------------------------------------------

func peerOf(n *gNode) string { return types.PeerPubKeyToID(types.PeerPublicKey(n.pub)).String() }

func callJSON(kind, group, room, id, coord string) string {
	s := `{"v":1,"k":"` + kind + `","g":"` + group + `","c":"` + id + `"`
	if room != "" {
		s += `,"r":"` + room + `"`
	}
	if coord != "" {
		s += `,"o":"` + coord + `"`
	}
	return s + `}`
}

// M1: a member cannot replace a call that is going on, nor end it. The call belongs to who started it, to
// its coordinator and to who may moderate; the others are heard in silence.
func TestGroupCallCannotBeReplacedOrEndedByAMember(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	c, err := alice.app.StartGroupCall(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	w.drain()
	ev, stop := callEvents(carol)
	defer stop()

	sendCallWire(t, w, bob, carol, callJSON("alive", id, "", "call_evil", peerOf(bob)))
	sendCallWire(t, w, bob, carol, callJSON("call", id, "", "call_evil2", peerOf(bob)))
	w.drain()
	got, ok := carol.app.ActiveGroupCall(id, "")
	if !ok || got.CallID != c.CallID || got.Coordinator != c.Coordinator {
		t.Fatalf("the call was replaced: %+v", got)
	}
	sendCallWire(t, w, bob, carol, callJSON("end", id, "", "call_evil", ""))
	sendCallWire(t, w, bob, carol, callJSON("end", id, "", c.CallID, ""))
	w.drain()
	if got, ok := carol.app.ActiveGroupCall(id, ""); !ok || got.CallID != c.CallID {
		t.Fatalf("a member ended the call: %+v %v", got, ok)
	}
	if len(ev.got) != 0 {
		t.Fatalf("the refused announcements made events: %+v", ev.got)
	}
	// a member still joins by repeating the call's own announcement
	sendCallWire(t, w, bob, carol, callJSON("alive", id, "", c.CallID, c.Coordinator))
	w.drain()
	if got, _ := carol.app.ActiveGroupCall(id, ""); len(got.Members) != 2 {
		t.Fatalf("a member could not join: %v", got.Members)
	}
	// who started it ends it
	if err := alice.app.EndGroupCall(ctx, id, "", c.CallID); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if _, ok := carol.app.ActiveGroupCall(id, ""); ok {
		t.Fatal("the initiator's end was not taken")
	}
}

func TestGroupCallCanBeEndedByAModerator(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	ctx := context.Background()
	c, _ := alice.app.StartGroupCall(ctx, id)
	if err := alice.app.GroupSetRole(ctx, id, bob.nodeHex(), "admin"); err != nil {
		t.Fatal(err)
	}
	w.drain()
	sendCallWire(t, w, bob, carol, callJSON("end", id, "", c.CallID, ""))
	w.drain()
	if _, ok := carol.app.ActiveGroupCall(id, ""); ok {
		t.Fatal("a moderator could not end the call")
	}
}

// the coordinator named at the first announcement may end the call too
func TestGroupCallCanBeEndedByItsCoordinator(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	sendCallWire(t, w, alice, carol, callJSON("call", id, "", "call_coord", peerOf(bob)))
	w.drain()
	if _, ok := carol.app.ActiveGroupCall(id, ""); !ok {
		t.Fatal("the call was not taken")
	}
	sendCallWire(t, w, bob, carol, callJSON("end", id, "", "call_coord", ""))
	w.drain()
	if _, ok := carol.app.ActiveGroupCall(id, ""); ok {
		t.Fatal("the coordinator could not end the call")
	}
}

// M2: one member does not ring the group over and over: a new call at most every 30 s.
func TestGroupCallsOfOneMemberAreSpacedOut(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ev, stop := callEvents(alice)
	defer stop()
	for i := 0; i < 50; i++ {
		sendCallWire(t, w, bob, alice, callJSON("call", id, "", "call_"+strings.Repeat("x", i+1), peerOf(bob)))
	}
	w.drain()
	if got := len(ev.of("call")); got != 1 {
		t.Fatalf("50 announcements rang %d times, want once", got)
	}
	// after the cooldown the member may start another call
	w.clk.off.Add(int64(31 * time.Second))
	sendCallWire(t, w, bob, alice, callJSON("call", id, "", "call_later", peerOf(bob)))
	w.drain()
	if got := len(ev.of("call")); got != 2 {
		t.Fatalf("after the cooldown: %d rings, want 2", got)
	}
	// ending a call gives the member the right to start the next at once
	sendCallWire(t, w, bob, alice, callJSON("end", id, "", "call_later", ""))
	sendCallWire(t, w, bob, alice, callJSON("call", id, "", "call_again", peerOf(bob)))
	w.drain()
	if got := len(ev.of("call")); got != 3 {
		t.Fatalf("after an end: %d rings, want 3", got)
	}
}

func TestGroupCallsAtOnceAreCappedPerGroup(t *testing.T) {
	groupWorldConfig = func(c *app.Config) { c.CallCooldown, c.MaxGroupCalls = -1, 2 }
	defer func() { groupWorldConfig = nil }()
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	for _, r := range []string{"r1", "r2", "r3"} {
		if _, err := alice.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: r, Name: r, Voice: true}); err != nil {
			t.Fatal(err)
		}
	}
	w.drain()
	ev, stop := callEvents(alice)
	defer stop()
	for _, r := range []string{"r1", "r2", "r3"} {
		sendCallWire(t, w, bob, alice, callJSON("alive", id, r, "call_"+r, peerOf(bob)))
	}
	w.drain()
	if got := len(ev.of("callactive")); got != 2 {
		t.Fatalf("%d calls were shown with a cap of 2: %+v", got, ev.got)
	}
	if _, ok := alice.app.ActiveGroupCall(id, "r3"); ok {
		t.Fatal("a call beyond the cap is active")
	}
}

// L1: the proxy has no password, so it listens on the loopback only.
func TestVPNListenMustBeLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:1080", "[::]:1080", ":1080", "192.168.1.5:1080", "example.org:1080", "127.0.0.1", "garbage"} {
		r := newRig(t, app.ModePublic)
		_, err := app.New(app.Config{Runtime: r.rt, Links: r.ln, Store: r.kv, Clock: clock{}, Self: nid(1), Exit: &fakeDialer{}, VPNListen: addr})
		if !errors.Is(err, app.ErrVPNListen) {
			t.Errorf("%q: %v", addr, err)
		}
	}
	for _, addr := range []string{"", "127.0.0.1:1080", "127.0.0.1:0", "[::1]:1080", "localhost:1080"} {
		r := newRig(t, app.ModePublic)
		if _, err := app.New(app.Config{Runtime: r.rt, Links: r.ln, Store: r.kv, Clock: clock{}, Self: nid(1), Exit: &fakeDialer{}, VPNListen: addr}); err != nil {
			t.Errorf("%q: %v", addr, err)
		}
	}
	if app.UserMessage(app.ErrVPNListen) == app.UserMessage(errors.New("x")) {
		t.Error("no user text for the refusal")
	}
}
