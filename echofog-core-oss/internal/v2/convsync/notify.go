package convsync

import (
	"encoding/binary"
	"errors"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Mode is what a channel may tell the user about: a local setting, kept in the device's store. It is
// not in the log and never leaves the device.
type Mode uint8

const (
	ModeAll      Mode = iota // every message
	ModeMentions             // only messages that name the user, and replies in followed threads
	ModeNone                 // nothing but being named
)

// Alert is how loud a message is.
type Alert uint8

const (
	AlertNone  Alert = iota // not shown as news
	AlertQuiet              // counted, no sound
	AlertLoud               // important: sound, whatever the mode
)

// threadScan is how many messages of a channel the thread list looks through.
const threadScan = 1000

// Prefs holds the notification settings of one device: the mode of each channel and the threads the
// user follows. Nothing here is written to the conversation log or sent to anybody.
type Prefs struct{ kv store.KV }

// NewPrefs opens the settings in kv.
func NewPrefs(kv store.KV) *Prefs { return &Prefs{kv: kv} }

func modeKey(conv convlog.ConvID, channel string) []byte {
	return []byte("mode/" + conv.String() + "/" + channel)
}

func muteKey(conv convlog.ConvID, channel string) []byte {
	return []byte("muteuntil/" + conv.String() + "/" + channel)
}

// SetMuteUntil silences a channel until the unix time until; 0 clears it. Like the mode, it is local.
func (p *Prefs) SetMuteUntil(conv convlog.ConvID, channel string, until int64) error {
	if until <= 0 {
		return p.kv.Delete(muteKey(conv, channel))
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(until))
	return p.kv.Put(muteKey(conv, channel), b[:])
}

// MuteUntil is the end of a channel's silence in unix seconds (0: none was set). Whether it has run
// out is for the caller, who has the clock.
func (p *Prefs) MuteUntil(conv convlog.ConvID, channel string) int64 {
	b, err := p.kv.Get(muteKey(conv, channel))
	if err != nil || len(b) != 8 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(b))
}

func followKey(conv string, root string) []byte { return []byte("follow/" + conv + "/" + root) }

// SetMode sets what channel ("" is the main one) of a conversation tells the user about.
func (p *Prefs) SetMode(conv convlog.ConvID, channel string, m Mode) error {
	if m > ModeNone {
		return errors.New("convsync: unknown notification mode")
	}
	if m == ModeAll {
		return p.kv.Delete(modeKey(conv, channel)) // the default needs no record
	}
	return p.kv.Put(modeKey(conv, channel), []byte{byte(m)})
}

// Mode is the setting of a channel (ModeAll when none was made).
func (p *Prefs) Mode(conv convlog.ConvID, channel string) Mode {
	b, err := p.kv.Get(modeKey(conv, channel))
	if err != nil || len(b) != 1 || Mode(b[0]) > ModeNone {
		return ModeAll
	}
	return Mode(b[0])
}

// Follow subscribes to the thread under message root, or leaves it.
func (p *Prefs) Follow(conv convlog.ConvID, root convlog.EventID, on bool) error {
	k := followKey(conv.String(), root.String())
	if !on {
		return p.kv.Delete(k)
	}
	return p.kv.Put(k, []byte{1})
}

// Following reports whether the user follows the thread under root.
func (p *Prefs) Following(conv convlog.ConvID, root convlog.EventID) bool {
	return p.followingHex(conv.String(), root.String())
}

func (p *Prefs) followingHex(conv, root string) bool {
	ok, err := p.kv.Has(followKey(conv, root))
	return err == nil && ok
}

// Alert says how loud a new message is for this user. A message that names the user, and a reply in a
// thread the user follows, is important whatever the mode; the rest follows the channel's mode. The
// user's own messages are no news.
func (p *Prefs) Alert(m convlog.Message) Alert {
	if m.Mine {
		return AlertNone
	}
	if m.Mentioned || m.Thread != "" && p.followingHex(m.Conv, m.Thread) {
		return AlertLoud
	}
	var conv convlog.ConvID
	if !parseConv(m.Conv, &conv) || p.Mode(conv, m.Channel) != ModeAll {
		return AlertNone
	}
	return AlertQuiet
}

func parseConv(s string, out *convlog.ConvID) bool {
	if len(s) != 2*len(out) {
		return false
	}
	for i := range out {
		hi, ok1 := unhex(s[2*i])
		lo, ok2 := unhex(s[2*i+1])
		if !ok1 || !ok2 {
			return false
		}
		out[i] = hi<<4 | lo
	}
	return true
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

// Thread is a thread of a channel as the list shows it.
type Thread struct {
	Root      convlog.Message
	Following bool
}

// Threads lists the threads of a channel with whether the user follows each.
func (p *Prefs) Threads(l *convlog.Log, c convlog.Conv, channel string) []Thread {
	roots := l.Threads(c, channel, threadScan)
	out := make([]Thread, 0, len(roots))
	for _, r := range roots {
		out = append(out, Thread{Root: r, Following: p.followingHex(c.ID.String(), r.ID)})
	}
	return out
}

// maxFollowed bounds the followed threads one list returns.
const maxFollowed = 1000

// Followed lists the roots of the threads of a conversation the user follows, in key order, up to
// maxFollowed. A thread can be followed before it has a reply, so this is not the thread list.
func (p *Prefs) Followed(conv convlog.ConvID) []string {
	prefix := "follow/" + conv.String() + "/"
	es, err := p.kv.Scan([]byte(prefix), maxFollowed)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, string(e.Key[len(prefix):]))
	}
	return out
}
