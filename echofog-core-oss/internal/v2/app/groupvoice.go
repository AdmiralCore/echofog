package app

import "errors"

// ErrNoVoice reports a voice-room call on an app built without a voice roster.
var ErrNoVoice = errors.New("app: voice rooms are not available")

// VoiceMember is one member present in a voice room.
type VoiceMember struct {
	Node   string
	Muted  bool
	Camera bool
	Stream bool
}

// VoiceRooms is the live state of the voice rooms (T7.7: who is in the room, from the calls of the
// group's sessions, never from the log). The runtime wires it (T11.3d).
type VoiceRooms interface {
	Roster(group, channel string) []VoiceMember
}

// GroupVoice lists who is in a group's voice room now. A member who has no live session with the
// room is not seen.
func (a *App) GroupVoice(id, channel string) ([]VoiceMember, error) {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return nil, err
	}
	if a.cfg.Voice == nil {
		return nil, ErrNoVoice
	}
	if !cv.Log().Participant(c, cv.Log().Self()) {
		return nil, ErrNoGroup
	}
	ch, ok := cv.Log().ChannelOf(c, channel)
	if !ok || ch.Removed || !ch.Voice {
		return nil, ErrNoGroup
	}
	return a.cfg.Voice.Roster(id, channel), nil
}
