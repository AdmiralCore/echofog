package convsync

import (
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

// Prefs holds the notification settings of one device.
type Prefs struct{ kv store.KV }

// NewPrefs opens the settings in kv.
func NewPrefs(kv store.KV) *Prefs { return &Prefs{kv: kv} }

// SetMode sets what channel ("" is the main one) of a conversation tells the user about.
func (p *Prefs) SetMode(conv convlog.ConvID, channel string, m Mode) error { return nil }

// Mode is the setting of a channel (ModeAll when none was made).
func (p *Prefs) Mode(conv convlog.ConvID, channel string) Mode { return ModeNone }

// Follow subscribes to the thread under message root, or leaves it.
func (p *Prefs) Follow(conv convlog.ConvID, root convlog.EventID, on bool) error { return nil }

// Following reports whether the user follows the thread under root.
func (p *Prefs) Following(conv convlog.ConvID, root convlog.EventID) bool { return false }

// Alert says how loud a new message is for this user.
func (p *Prefs) Alert(m convlog.Message) Alert { return AlertNone }

// Thread is a thread of a channel as the list shows it.
type Thread struct {
	Root      convlog.Message
	Following bool
}

// Threads lists the threads of a channel with whether the user follows each.
func (p *Prefs) Threads(l *convlog.Log, c convlog.Conv, channel string) []Thread { return nil }
