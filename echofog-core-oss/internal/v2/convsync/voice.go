package convsync

import (
	"context"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
)

// Voice-room roster tuning.
const (
	// RosterTTL is how long a participant stays listed without a sign of life.
	RosterTTL = 30 * time.Second
	// RosterBeat is how often a participant in a room repeats its state.
	RosterBeat = 10 * time.Second
	// MaxRoomsPerGroup bounds the rooms of a group one peer may be listed in.
	MaxRoomsPerPeer = 4
)

// LiveNet sends a payload over a live session to the account behind node, and nowhere else: no outbox,
// no mailbox. It fails when there is no session.
type LiveNet interface {
	SendLive(ctx context.Context, to [32]byte, payload []byte) error
}

// VoiceState is what a participant of a room shows.
type VoiceState struct{ Mic, Cam, Stream bool }

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
}

// VoiceRoster is who is in the voice rooms of the groups we are in, from live state only.
type VoiceRoster struct{ cfg VoiceConfig }

// NewVoiceRoster builds a roster.
func NewVoiceRoster(cfg VoiceConfig) *VoiceRoster { return &VoiceRoster{cfg: cfg} }

// Join enters a room (or changes what we show) and tells the group's members who have a session.
func (v *VoiceRoster) Join(ctx context.Context, c convlog.Conv, room string, st VoiceState) error {
	return nil
}

// Leave leaves a room.
func (v *VoiceRoster) Leave(ctx context.Context, c convlog.Conv, room string) error { return nil }

// Receive takes a roster message from the account behind node.
func (v *VoiceRoster) Receive(ctx context.Context, from [32]byte, payload []byte) bool { return false }

// Tick repeats our state and drops those not heard from.
func (v *VoiceRoster) Tick(ctx context.Context) error { return nil }

// Who lists a room. complete is false while the group's membership is not known here yet: the list
// must not be shown as final then.
func (v *VoiceRoster) Who(c convlog.Conv, room string) (list []Participant, complete bool) {
	return nil, true
}
