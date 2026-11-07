package convsync

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"google.golang.org/protobuf/proto"
)

// ---- live sessions between the members, delivered when the test says so --------------------------------

type liveLetter struct {
	from, to [32]byte
	payload  []byte
}

type liveNet struct {
	mu    sync.Mutex
	queue []liveLetter
	rost  map[[32]byte]*VoiceRoster
	down  map[[32]byte]bool
	sent  int
	all   [][]byte // everything ever sent
}

type livePort struct {
	n    *liveNet
	self [32]byte
}

func (p livePort) SendLive(ctx context.Context, to [32]byte, payload []byte) error {
	p.n.mu.Lock()
	defer p.n.mu.Unlock()
	p.n.sent++
	p.n.all = append(p.n.all, append([]byte(nil), payload...))
	if p.n.down[to] || p.n.down[p.self] {
		return errors.New("no session")
	}
	p.n.queue = append(p.n.queue, liveLetter{p.self, to, append([]byte(nil), payload...)})
	return nil
}

func (n *liveNet) drain(t testing.TB) {
	t.Helper()
	for i := 0; i < 100000; i++ {
		n.mu.Lock()
		if len(n.queue) == 0 {
			n.mu.Unlock()
			return
		}
		l := n.queue[0]
		n.queue = n.queue[1:]
		r := n.rost[l.to]
		n.mu.Unlock()
		if r != nil {
			r.Receive(context.Background(), l.from, l.payload)
		}
	}
	t.Fatal("the live network never went quiet")
}

type voiceWorld struct {
	*cluster
	net   *liveNet
	rost  []*VoiceRoster
	now   time.Time
	nowMu sync.Mutex
}

func (w *voiceWorld) clock() time.Time { w.nowMu.Lock(); defer w.nowMu.Unlock(); return w.now }
func (w *voiceWorld) advance(d time.Duration) {
	w.nowMu.Lock()
	w.now = w.now.Add(d)
	w.nowMu.Unlock()
}

// newVoiceWorld makes a group of size members with a voice room "lounge" known to all.
func newVoiceWorld(t testing.TB, size int, roles map[int]string) *voiceWorld {
	t.Helper()
	w := &voiceWorld{cluster: newCluster(t, size, size), now: time.Unix(1_800_000_000, 0)}
	w.net = &liveNet{rost: map[[32]byte]*VoiceRoster{}, down: map[[32]byte]bool{}}
	w.rosterOf(size, roles)
	ch, _, err := w.members[0].e.Log().AppendChannel(w.group, convlog.ChannelSpec{ID: "lounge", Name: "lounge", Voice: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < size; i++ {
		if _, _, err := w.members[i].e.Log().Ingest(w.group, ch); err != nil {
			t.Fatal(err)
		}
	}
	var pubs [][32]byte
	for _, m := range w.members {
		pubs = append(pubs, m.a.pub)
	}
	for _, m := range w.members {
		m := m
		r := NewVoiceRoster(VoiceConfig{
			Pub: m.a.pub, Log: m.e.Log(), Dir: w.n, Net: livePort{w.net, m.a.node},
			Groups:  func(id convlog.ConvID) (convlog.Conv, bool) { return w.group, id == w.group.ID },
			Members: func(convlog.Conv) [][32]byte { return pubs },
			Now:     w.clock,
		})
		w.rost = append(w.rost, r)
		w.net.rost[m.a.node] = r
	}
	return w
}

func (w *voiceWorld) who(i int) map[[32]byte]VoiceState {
	list, _ := w.rost[i].Who(w.group, "lounge")
	out := map[[32]byte]VoiceState{}
	for _, p := range list {
		out[p.Pub] = p.State
	}
	return out
}

// stageOf adds a voice channel "stage" in which everybody is denied the voice right.
func (w *voiceWorld) stage(t *testing.T) {
	t.Helper()
	ch, _, err := w.members[0].e.Log().AppendChannel(w.group, convlog.ChannelSpec{ID: "stage", Name: "stage", Voice: true,
		Overrides: []convlog.Override{{Role: convlog.Everyone, Deny: convlog.PermVoice}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(w.members); i++ {
		if _, _, err := w.members[i].e.Log().Ingest(w.group, ch); err != nil {
			t.Fatal(err)
		}
	}
}

func (w *voiceWorld) join(t *testing.T, i int, st VoiceState) {
	t.Helper()
	if err := w.rost[i].Join(context.Background(), w.group, "lounge", st); err != nil {
		t.Fatal(err)
	}
}

// ---- tests ---------------------------------------------------------------------------------------------

// The roster of a room agrees on every member's side right after people come and go, with no clock
// moving and nothing written to the log.
func TestVoiceRosterConvergesAndStaysOutOfTheLog(t *testing.T) {
	w := newVoiceWorld(t, 5, nil)
	before := make([]string, 5)
	for i, m := range w.members {
		meta, _ := m.e.Log().Meta(w.group)
		before[i] = string(meta.Digest())
	}
	letters := w.cluster.n.sent

	w.join(t, 1, VoiceState{Mic: true})
	w.join(t, 2, VoiceState{Mic: true, Cam: true})
	w.join(t, 3, VoiceState{})
	w.net.drain(t)
	for i := 0; i < 5; i++ {
		got := w.who(i)
		if len(got) != 3 || !got[w.members[2].a.pub].Cam || !got[w.members[1].a.pub].Mic || got[w.members[3].a.pub].Mic {
			t.Fatalf("member %d sees %+v", i, got)
		}
	}
	// somebody starts a stream, somebody leaves
	w.join(t, 3, VoiceState{Mic: true, Stream: true})
	if err := w.rost[2].Leave(context.Background(), w.group, "lounge"); err != nil {
		t.Fatal(err)
	}
	w.net.drain(t)
	for i := 0; i < 5; i++ {
		got := w.who(i)
		if len(got) != 2 || !got[w.members[3].a.pub].Stream {
			t.Fatalf("member %d sees %+v after the change", i, got)
		}
		if _, in := got[w.members[2].a.pub]; in {
			t.Fatalf("member %d still lists the one who left", i)
		}
	}
	for i, m := range w.members {
		meta, _ := m.e.Log().Meta(w.group)
		if string(meta.Digest()) != before[i] {
			t.Fatalf("member %d: the log changed", i)
		}
	}
	if w.cluster.n.sent != letters {
		t.Fatal("the roster went over the message path (outbox, mailbox)")
	}
}

// Somebody with no session is not listed, nothing waits for them, and they catch up from the next beat.
func TestVoiceRosterNeedsALiveSessionAndCatchesUp(t *testing.T) {
	w := newVoiceWorld(t, 4, nil)
	late := w.members[3].a.node
	w.net.down[late] = true
	w.join(t, 1, VoiceState{Mic: true})
	w.net.drain(t)
	if len(w.who(3)) != 0 {
		t.Fatal("somebody without a session sees the room")
	}
	if len(w.net.queue) != 0 {
		t.Fatal("something waits for the one who is offline")
	}
	delete(w.net.down, late)
	w.advance(RosterBeat + time.Second)
	for _, r := range w.rost {
		_ = r.Tick(context.Background())
	}
	w.net.drain(t)
	if got := w.who(3); len(got) != 1 || !got[w.members[1].a.pub].Mic {
		t.Fatalf("after the next beat: %+v", got)
	}
}

// A stranger, a member without the voice right and a payload for a room that is no voice room are ignored.
func TestVoiceRosterIgnoresThoseWhoMayNotBeThere(t *testing.T) {
	w := newVoiceWorld(t, 4, map[int]string{3: "subscriber"})
	// a reader has no voice right: its own roster refuses, and a message it makes by hand is dropped by the others
	if err := w.rost[3].Join(context.Background(), w.group, "lounge", VoiceState{Mic: true}); err != ErrNoVoice {
		t.Fatalf("a reader joined a voice room: %v", err)
	}
	forged := encodeRoster(rosterState, w.group.ID, 1, VoiceState{Mic: true}, "lounge")
	for _, i := range []int{0, 1, 2} {
		w.rost[i].Receive(context.Background(), w.members[3].a.node, forged)
	}
	if len(w.who(0)) != 0 || len(w.who(1)) != 0 {
		t.Fatal("a member without the voice right is listed")
	}
	// a room that is not a voice channel
	_ = w.rost[1].Join(context.Background(), w.group, "nowhere", VoiceState{})
	w.net.drain(t)
	if list, _ := w.rost[0].Who(w.group, "nowhere"); len(list) != 0 {
		t.Fatal("a room that does not exist has people in it")
	}
	// somebody outside the group: its payload is not taken
	outsider := newAcct(t)
	w.cluster.n.pubs[outsider.node] = outsider.pub
	w.rost[0].Receive(context.Background(), outsider.node, encodeRoster(rosterState, w.group.ID, 1, VoiceState{Mic: true}, "lounge"))
	if len(w.who(0)) != 0 {
		t.Fatal("an outsider is listed")
	}
	// a payload that is not a roster message is not one
	for _, junk := range [][]byte{nil, {1}, make([]byte, 28), make([]byte, 61)} {
		if w.rost[0].Receive(context.Background(), w.members[1].a.node, junk) {
			t.Fatalf("%d bytes of junk taken for a roster message", len(junk))
		}
	}
}

// Without a sign of life a participant drops out; an old state replayed after a leave does not bring
// them back.
func TestVoiceRosterExpiresAndRefusesReplays(t *testing.T) {
	w := newVoiceWorld(t, 3, nil)
	w.join(t, 1, VoiceState{Mic: true})
	if len(w.net.all) == 0 {
		t.Fatal("the join was not sent")
	}
	join := w.net.all[0]
	w.net.drain(t)
	if len(w.who(0)) != 1 {
		t.Fatal("not listed")
	}
	if err := w.rost[1].Leave(context.Background(), w.group, "lounge"); err != nil {
		t.Fatal(err)
	}
	w.net.drain(t)
	w.rost[0].Receive(context.Background(), w.members[1].a.node, join) // the old join, again
	if len(w.who(0)) != 0 {
		t.Fatal("a replayed join brought a participant back")
	}
	w.join(t, 1, VoiceState{})
	w.net.drain(t)
	if len(w.who(0)) != 1 {
		t.Fatal("a fresh join after a leave was refused")
	}
	// silence
	w.net.down[w.members[1].a.node] = true
	w.advance(RosterTTL + time.Second)
	_ = w.rost[0].Tick(context.Background())
	if len(w.who(0)) != 0 {
		t.Fatal("a participant who went silent is still listed")
	}
}

// Until the group's membership is known here the list is not final.
func TestVoiceRosterIsNotFinalWhileTheMembershipIsIncomplete(t *testing.T) {
	c := newCluster(t, 3, 3)
	late := c.members[2].a.node
	c.n.cut = func(from, to [32]byte) bool { return to == late || from == late }
	c.rosterOf(3, nil)
	pubs := [][32]byte{c.members[0].a.pub, c.members[1].a.pub, c.members[2].a.pub}
	r := NewVoiceRoster(VoiceConfig{
		Pub: c.members[2].a.pub, Log: c.members[2].e.Log(), Dir: c.n, Net: livePort{&liveNet{}, late},
		Groups:  func(id convlog.ConvID) (convlog.Conv, bool) { return c.group, id == c.group.ID },
		Members: func(convlog.Conv) [][32]byte { return pubs },
		Now:     func() time.Time { return time.Unix(1_800_000_000, 0) },
	})
	if _, complete := r.Who(c.group, "lounge"); complete {
		t.Fatal("a member that never got the roster shows the room as final")
	}
	// the roster reaches the member (an invitation carries it; a sync brings it once the group service knows the member)
	if _, _, err := c.members[2].e.Log().Ingest(c.group, c.roster); err != nil {
		t.Fatal(err)
	}
	if _, complete := r.Who(c.group, "lounge"); !complete {
		t.Fatal("still incomplete after the sync")
	}
}

// Whatever a peer sends, nothing panics and the list stays within its bounds.
func FuzzVoiceReceive(f *testing.F) {
	w := newVoiceWorld(f, 3, nil)
	f.Add(encodeRoster(rosterState, w.group.ID, 5, VoiceState{Mic: true}, "lounge"))
	f.Add(encodeRoster(rosterLeave, w.group.ID, 6, VoiceState{}, "lounge"))
	f.Add([]byte{})
	f.Add(bytesOf(0xff, 60))
	f.Fuzz(func(t *testing.T, b []byte) {
		w.rost[0].Receive(context.Background(), w.members[1].a.node, b)
		if list, _ := w.rost[0].Who(w.group, "lounge"); len(list) > 2 {
			t.Fatalf("%d participants in a room of a group of three", len(list))
		}
	})
}

// The right to join is the one the room's channel gives (its overrides count), not the group's.
func TestVoiceRosterFollowsTheChannelOverrides(t *testing.T) {
	w := newVoiceWorld(t, 3, nil)
	w.stage(t)
	if err := w.rost[1].Join(context.Background(), w.group, "stage", VoiceState{Mic: true}); err != ErrNoVoice {
		t.Fatalf("joined a room whose channel denies voice: %v", err)
	}
	// a message made by hand is dropped by the others, who read the channel's rights
	forged := encodeRoster(rosterState, w.group.ID, 1, VoiceState{Mic: true}, "stage")
	w.rost[0].Receive(context.Background(), w.members[1].a.node, forged)
	w.rost[2].Receive(context.Background(), w.members[1].a.node, forged)
	for _, i := range []int{0, 2} {
		if list, _ := w.rost[i].Who(w.group, "stage"); len(list) != 0 {
			t.Fatalf("member %d lists somebody the channel does not let in", i)
		}
	}
	// the other room is not affected
	w.join(t, 1, VoiceState{})
	w.net.drain(t)
	if len(w.who(0)) != 1 {
		t.Fatal("the ordinary room stopped working")
	}
}

// Somebody removed from the group is no longer listed, at once (not after the TTL).
func TestVoiceRosterDropsTheRemovedAtOnce(t *testing.T) {
	w := newVoiceWorld(t, 3, nil)
	w.join(t, 1, VoiceState{Mic: true})
	w.net.drain(t)
	if len(w.who(0)) != 1 {
		t.Fatal("not listed")
	}
	body, _ := proto.Marshal(&convlog.MemberBody{Action: convlog.MemberRemove, Member: w.members[1].a.pub[:]})
	rm, _, err := w.members[0].e.Log().AppendKept(w.group, convlog.Draft{Kind: convlog.KindMember, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.members[2].e.Log().Ingest(w.group, rm); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 2} {
		if got := w.who(i); len(got) != 0 {
			t.Fatalf("member %d still lists the removed one: %+v", i, got)
		}
	}
}

// One group cannot use up the places of the others: the ceiling is per group.
func TestVoiceRosterCeilingIsPerGroup(t *testing.T) {
	c := newCluster(t, 4, 4)
	c.rosterOf(4, nil)
	ch, _, err := c.members[0].e.Log().AppendChannel(c.group, convlog.ChannelSpec{ID: "lounge", Name: "lounge", Voice: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.members[3].e.Log().Ingest(c.group, ch); err != nil {
		t.Fatal(err)
	}
	r := NewVoiceRoster(VoiceConfig{
		Pub: c.members[3].a.pub, Log: c.members[3].e.Log(), Dir: c.n, MaxPerGroup: 1,
		Groups: func(id convlog.ConvID) (convlog.Conv, bool) { return c.group, id == c.group.ID },
		Now:    func() time.Time { return time.Unix(1_800_000_000, 0) },
	})
	r.Receive(context.Background(), c.members[1].a.node, encodeRoster(rosterState, c.group.ID, 1, VoiceState{Mic: true}, "lounge"))
	r.Receive(context.Background(), c.members[2].a.node, encodeRoster(rosterState, c.group.ID, 1, VoiceState{Mic: true}, "lounge"))
	if list, _ := r.Who(c.group, "lounge"); len(list) != 1 {
		t.Fatalf("a ceiling of 1 left %d listed", len(list))
	}
}
