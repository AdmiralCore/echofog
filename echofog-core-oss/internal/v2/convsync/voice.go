package convsync

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/ratelimit"
	"github.com/echofog/echofog-core/internal/services/convlog"
)

// Voice-room roster (T7.7): who is in the voice rooms of a group, from live state only. It is not an
// event of the conversation log and never goes through the outbox or a mailbox, like "is typing": a
// member with no live session is simply not seen, and learns the room from the next beat.
//
// A message is a fixed layout, at most rosterMaxLen bytes:
//
//	[0]      version, 1
//	[1]      kind: 1 state (join or change), 2 leave
//	[2:18]   conversation id
//	[18:26]  sequence number, big endian: a millisecond clock that only rises, so that an old message
//	         replayed after a newer one is not taken
//	[26]     flags: 1 microphone, 2 camera, 4 stream
//	[27]     length n of the room id (1..32)
//	[28:]    the room id, a voice channel of the group
//
// Who sent it is the authenticated session the message came over, not a field of it.

// Voice-room roster tuning.
const (
	// RosterTTL is how long a participant stays listed without a sign of life.
	RosterTTL = 30 * time.Second
	// RosterBeat is how often a participant in a room repeats its state.
	RosterBeat = 10 * time.Second
	// MaxRoomsPerPeer bounds the rooms one peer may be listed in.
	MaxRoomsPerPeer = 4
	// MaxRosterPerGroup bounds the participants listed for one group, so that one group cannot use up the
	// places of the others.
	MaxRosterPerGroup = 1024
	// MaxRosterFanout bounds how many members one change is sent to.
	MaxRosterFanout = 200
	// tombstoneKeep is how long a leave is remembered, so that an older state is not taken after it.
	tombstoneKeep = 10 * time.Minute

	rosterVersion = 1
	rosterState   = 1
	rosterLeave   = 2
	rosterMaxLen  = 28 + 32
)

// Errors of the roster.
var (
	ErrNotInGroup = errors.New("convsync: not a group we are in")
	ErrNoRoom     = errors.New("convsync: no such voice room")
	ErrNoVoice    = errors.New("convsync: no right to join voice rooms")
)

// LiveNet sends a payload over a live session to the account behind node, and nowhere else: no outbox,
// no mailbox. It fails when there is no session.
type LiveNet interface {
	SendLive(ctx context.Context, to [32]byte, payload []byte) error
}

// VoiceState is what a participant of a room shows.
type VoiceState struct{ Mic, Cam, Stream bool }

func (s VoiceState) flags() byte {
	var f byte
	if s.Mic {
		f |= 1
	}
	if s.Cam {
		f |= 2
	}
	if s.Stream {
		f |= 4
	}
	return f
}

func stateOf(f byte) VoiceState { return VoiceState{Mic: f&1 != 0, Cam: f&2 != 0, Stream: f&4 != 0} }

// Participant is somebody in a room.
type Participant struct {
	Pub   [32]byte
	State VoiceState
}

// VoiceConfig wires a roster.
type VoiceConfig struct {
	Pub     [32]byte
	Log     *convlog.Log
	Dir     Directory
	Net     LiveNet
	Groups  func(convlog.ConvID) (convlog.Conv, bool)
	Members func(convlog.Conv) [][32]byte
	Now     func() time.Time
	// MaxPerGroup bounds the participants listed for one group (0: MaxRosterPerGroup).
	MaxPerGroup int
}

type roomKey struct {
	conv convlog.ConvID
	room string
}

type seat struct {
	state VoiceState
	seq   uint64
	seen  time.Time
	left  bool
}

type ownSeat struct {
	conv     convlog.Conv
	state    VoiceState
	seq      uint64
	lastSent time.Time
}

// VoiceRoster is who is in the voice rooms of the groups we are in.
type VoiceRoster struct {
	cfg   VoiceConfig
	now   func() time.Time
	limit *ratelimit.Keyed[[32]byte]

	mu      sync.Mutex
	seats   map[roomKey]map[[32]byte]*seat
	entries map[convlog.ConvID]int // listed places per group
	own     map[roomKey]*ownSeat
	lastSeq uint64
}

// NewVoiceRoster builds a roster.
func NewVoiceRoster(cfg VoiceConfig) *VoiceRoster {
	v := &VoiceRoster{
		cfg: cfg, now: cfg.Now,
		limit:   ratelimit.NewKeyed[[32]byte](5, 40, 4096),
		seats:   map[roomKey]map[[32]byte]*seat{},
		own:     map[roomKey]*ownSeat{},
		entries: map[convlog.ConvID]int{},
	}
	if v.cfg.MaxPerGroup <= 0 {
		v.cfg.MaxPerGroup = MaxRosterPerGroup
	}
	if v.now == nil {
		v.now = time.Now
	}
	return v
}

func encodeRoster(kind byte, conv convlog.ConvID, seq uint64, st VoiceState, room string) []byte {
	b := make([]byte, 0, 28+len(room))
	b = append(b, rosterVersion, kind)
	b = append(b, conv[:]...)
	b = binary.BigEndian.AppendUint64(b, seq)
	b = append(b, st.flags(), byte(len(room)))
	return append(b, room...)
}

type rosterMsg struct {
	kind  byte
	conv  convlog.ConvID
	seq   uint64
	state VoiceState
	room  string
}

// parseRoster reads a roster message; anything that is not exactly one is refused.
func parseRoster(b []byte) (rosterMsg, bool) {
	var m rosterMsg
	if len(b) < 28 || len(b) > rosterMaxLen || b[0] != rosterVersion || b[1] != rosterState && b[1] != rosterLeave {
		return m, false
	}
	n := int(b[27])
	if n < 1 || n > 32 || len(b) != 28+n || b[26] > 7 {
		return m, false
	}
	m.kind = b[1]
	copy(m.conv[:], b[2:18])
	m.seq = binary.BigEndian.Uint64(b[18:26])
	m.state = stateOf(b[26])
	m.room = string(b[28:])
	return m, true
}

// voiceRoom reports whether room is a voice channel of the group.
func (v *VoiceRoster) voiceRoom(c convlog.Conv, room string) bool {
	ch, ok := v.cfg.Log.ChannelOf(c, room)
	return ok && ch.Voice
}

// nextSeq is a clock that only rises. Call with v.mu held.
func (v *VoiceRoster) nextSeq() uint64 {
	s := uint64(v.now().UnixMilli())
	if s <= v.lastSeq {
		s = v.lastSeq + 1
	}
	v.lastSeq = s
	return s
}

// Join enters a room (or changes what we show) and tells the group's members who have a session.
func (v *VoiceRoster) Join(ctx context.Context, c convlog.Conv, room string, st VoiceState) error {
	if c.Kind != convlog.KindGroup {
		return ErrNotInGroup
	}
	if !v.voiceRoom(c, room) {
		return ErrNoRoom
	}
	if !v.cfg.Log.RightsIn(c, peerIDOf(v.cfg.Pub), room).Voice { // the channel's overrides count
		return ErrNoVoice
	}
	k := roomKey{c.ID, room}
	v.mu.Lock()
	o := v.own[k]
	if o == nil {
		o = &ownSeat{conv: c}
		v.own[k] = o
	}
	o.state, o.seq, o.lastSent = st, v.nextSeq(), v.now()
	payload := encodeRoster(rosterState, c.ID, o.seq, st, room)
	v.mu.Unlock()
	v.broadcast(ctx, c, payload)
	return nil
}

// Leave leaves a room.
func (v *VoiceRoster) Leave(ctx context.Context, c convlog.Conv, room string) error {
	k := roomKey{c.ID, room}
	v.mu.Lock()
	if _, in := v.own[k]; !in {
		v.mu.Unlock()
		return nil
	}
	delete(v.own, k)
	payload := encodeRoster(rosterLeave, c.ID, v.nextSeq(), VoiceState{}, room)
	v.mu.Unlock()
	v.broadcast(ctx, c, payload)
	return nil
}

// broadcast sends a payload to the members of c over live sessions. A member we cannot reach is skipped.
func (v *VoiceRoster) broadcast(ctx context.Context, c convlog.Conv, payload []byte) {
	if v.cfg.Net == nil || v.cfg.Members == nil {
		return
	}
	var others [][32]byte
	for _, m := range v.cfg.Members(c) {
		if m != v.cfg.Pub {
			others = append(others, m)
		}
	}
	if len(others) > MaxRosterFanout {
		others = pick(others, MaxRosterFanout)
	}
	for _, m := range others {
		_ = v.cfg.Net.SendLive(ctx, NodeOf(m), payload)
	}
}

// Receive takes a roster message from the account behind node. It reports whether the payload was a
// roster message at all; one from somebody who may not be in the room is dropped without an answer.
func (v *VoiceRoster) Receive(ctx context.Context, from [32]byte, payload []byte) bool {
	m, ok := parseRoster(payload)
	if !ok {
		return false
	}
	if !v.limit.AllowAt(from, 1, v.now()) {
		return true
	}
	c, ok := v.cfg.Groups(m.conv)
	if !ok || c.Kind != convlog.KindGroup {
		return true
	}
	pub, err := v.cfg.Dir.Pub(ctx, from)
	if err != nil || pub == v.cfg.Pub {
		return true
	}
	if !v.voiceRoom(c, m.room) || !v.cfg.Log.RightsIn(c, peerIDOf(pub), m.room).Voice {
		return true
	}
	k := roomKey{m.conv, m.room}
	now := v.now()
	v.mu.Lock()
	room := v.seats[k]
	if room == nil {
		room = map[[32]byte]*seat{}
		v.seats[k] = room
	}
	s := room[pub]
	if s != nil && m.seq <= s.seq {
		v.mu.Unlock()
		return true // older than what we hold: a replay or a late copy
	}
	if m.kind == rosterLeave {
		if s != nil {
			s.left, s.seq, s.seen, s.state = true, m.seq, now, VoiceState{}
		}
		v.mu.Unlock()
		return true
	}
	fresh := s == nil || s.left || now.Sub(s.seen) > RosterTTL
	if s == nil {
		if v.entries[m.conv] >= v.cfg.MaxPerGroup || v.roomsOf(pub) >= MaxRoomsPerPeer {
			v.mu.Unlock()
			return true
		}
		s = &seat{}
		room[pub] = s
		v.entries[m.conv]++
	}
	s.state, s.seq, s.seen, s.left = m.state, m.seq, now, false
	var reply []byte
	if o := v.own[k]; o != nil && fresh {
		reply = encodeRoster(rosterState, m.conv, v.nextSeq(), o.state, m.room)
	}
	v.mu.Unlock()
	if reply != nil && v.cfg.Net != nil {
		_ = v.cfg.Net.SendLive(ctx, from, reply) // the newcomer learns who is here
	}
	return true
}

// roomsOf counts the rooms pub is listed in. Call with v.mu held.
func (v *VoiceRoster) roomsOf(pub [32]byte) int {
	n := 0
	for _, room := range v.seats {
		if s := room[pub]; s != nil && !s.left {
			n++
		}
	}
	return n
}

// Tick repeats our state when it is time and drops those long gone.
func (v *VoiceRoster) Tick(ctx context.Context) error {
	now := v.now()
	type resend struct {
		c       convlog.Conv
		payload []byte
	}
	var out []resend
	v.mu.Lock()
	for k, room := range v.seats {
		for pub, s := range room {
			age := now.Sub(s.seen)
			if s.left && age > tombstoneKeep || !s.left && age > RosterTTL+tombstoneKeep {
				delete(room, pub)
				v.entries[k.conv]--
			}
		}
		if len(room) == 0 {
			delete(v.seats, k)
		}
	}
	for k, o := range v.own {
		if now.Sub(o.lastSent) >= RosterBeat {
			o.seq, o.lastSent = v.nextSeq(), now
			out = append(out, resend{o.conv, encodeRoster(rosterState, k.conv, o.seq, o.state, k.room)})
		}
	}
	v.mu.Unlock()
	for _, r := range out {
		v.broadcast(ctx, r.c, r.payload)
	}
	return nil
}

// Who lists a room. complete is false while the group's membership is not known here yet: the list
// must not be shown as final then (a large group catches its roster up through the sync).
func (v *VoiceRoster) Who(c convlog.Conv, room string) (list []Participant, complete bool) {
	complete = v.cfg.Log.HasRoster(c)
	now := v.now()
	v.mu.Lock()
	if o := v.own[roomKey{c.ID, room}]; o != nil {
		list = append(list, Participant{Pub: v.cfg.Pub, State: o.state})
	}
	var heard []Participant
	for pub, s := range v.seats[roomKey{c.ID, room}] {
		if !s.left && now.Sub(s.seen) <= RosterTTL {
			heard = append(heard, Participant{Pub: pub, State: s.state})
		}
	}
	v.mu.Unlock()
	// the right is read again now: somebody removed or denied since is not listed, whatever their last beat
	for _, p := range heard {
		if v.cfg.Log.RightsIn(c, peerIDOf(p.Pub), room).Voice {
			list = append(list, p)
		}
	}
	return list, complete
}
