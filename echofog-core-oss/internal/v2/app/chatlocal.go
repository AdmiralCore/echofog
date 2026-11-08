package app

import (
	"context"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
)

// What this device knows about a chat by itself, from the log and the reader's own marks: the silence
// for a term, the news of each channel, and which messages are new. None of it is sent anywhere (T11.3e).

// maxMute is the longest silence a chat can be given.
const maxMute = 365 * 24 * time.Hour

// ChatMuteUntil silences a chat's channel until the unix time until (0 or a past time clears it).
func (a *App) ChatMuteUntil(ref ChatRef, until int64) error {
	p, err := a.prefs()
	if err != nil {
		return err
	}
	c, err := a.resolve(context.Background(), ref, false)
	if err != nil {
		return err
	}
	now := a.cfg.Clock.Now()
	if until <= now.Unix() {
		return p.SetMuteUntil(c.ID, ref.Channel, 0)
	}
	if until > now.Add(maxMute).Unix() {
		return ErrRequest
	}
	return p.SetMuteUntil(c.ID, ref.Channel, until)
}

// ChatMutedUntil is the end of the silence of a chat's channel in unix seconds; 0 when it is not
// silenced (a silence that has run out is gone).
func (a *App) ChatMutedUntil(ref ChatRef) (int64, error) {
	p, err := a.prefs()
	if err != nil {
		return 0, err
	}
	c, err := a.resolve(context.Background(), ref, false)
	if err != nil {
		return 0, err
	}
	return a.mutedUntil(p, c.ID, ref.Channel), nil
}

func (a *App) mutedUntil(p interface {
	MuteUntil(convlog.ConvID, string) int64
}, conv convlog.ConvID, channel string) int64 {
	u := p.MuteUntil(conv, channel)
	if u > a.cfg.Clock.Now().Unix() {
		return u
	}
	return 0
}

// ChannelStat is where one channel of a group stands for this reader: unread messages, those that name
// the reader, and the last message with its time. It comes from the log and the reader's own marks.
type ChannelStat struct {
	Unread, Mentions int
	LastTS           int64 // ms
	Last             *convlog.Message
}

// GroupChannelStats gives a ChannelStat for the main channel ("") and for each channel with news.
func (a *App) GroupChannelStats(id string) (map[string]ChannelStat, error) {
	_, cv, c, _, err := a.grp(id)
	if err != nil {
		return nil, err
	}
	meta, _ := cv.Log().Meta(c)
	last := func(lastID string) *convlog.Message {
		if lastID == "" {
			return nil
		}
		eid, err := convlog.ParseEventID(lastID)
		if err != nil {
			return nil
		}
		m, ok := cv.Log().Message(c, eid)
		if !ok {
			return nil
		}
		m = a.dress([]convlog.Message{m})[0]
		return &m
	}
	out := map[string]ChannelStat{"": {Unread: meta.Unread, Mentions: meta.Mentions, LastTS: meta.LastTS, Last: last(meta.LastID)}}
	for ch, st := range meta.Chans {
		if st == nil {
			continue
		}
		out[ch] = ChannelStat{Unread: st.Unread, Mentions: st.Mentions, LastTS: st.LastTS, Last: last(st.LastID)}
	}
	return out, nil
}

// maxSeen bounds the messages remembered as told.
const maxSeen = 8192

// seenSet remembers the ids of the messages already told as new, the oldest forgotten first.
type seenSet struct {
	mu    sync.Mutex
	set   map[string]struct{}
	order []string
}

// firstTime reports whether id is new to the set and adds it.
func (s *seenSet) firstTime(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[id]; ok {
		return false
	}
	if s.set == nil {
		s.set = map[string]struct{}{}
	}
	s.set[id] = struct{}{}
	s.order = append(s.order, id)
	if len(s.order) > maxSeen {
		delete(s.set, s.order[0])
		s.order = s.order[1:]
	}
	return true
}

// freshOf picks from the messages of a change those that are new to the reader. An edit, a deletion, a
// reaction, a pin, a title of a thread or a reply to a message makes the message change without being
// news: it is shown again (conv:change) and neither alerts nor counts. A message is told once.
func (a *App) freshOf(msgs []convlog.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Edited || m.Deleted || m.Withheld || m.Pinned || len(m.Reactions) > 0 || m.Replies > 0 || m.ThreadTitle != "" {
			continue
		}
		if a.seen.firstTime(m.ID) {
			out = append(out, m.ID)
		}
	}
	return out
}
