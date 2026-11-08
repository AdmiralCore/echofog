package app

import (
	"errors"

	"github.com/echofog/echofog-core/internal/services/convlog"
)

// ChatMuteUntil silences a chat's channel until the unix time until (0 or a past time clears it).
func (a *App) ChatMuteUntil(ref ChatRef, until int64) error { return errors.New("not built") }

// ChatMutedUntil is the end of the silence of a chat's channel in unix seconds; 0 when it is not silenced.
func (a *App) ChatMutedUntil(ref ChatRef) (int64, error) { return 0, errors.New("not built") }

// ChannelStat is where one channel of a group stands for this reader: unread messages, those that name
// the reader, and the last message with its time. It comes from the log and the reader's own marks.
type ChannelStat struct {
	Unread, Mentions int
	LastTS           int64 // ms
	Last             *convlog.Message
}

// GroupChannelStats gives a ChannelStat for the main channel ("") and for each channel with news.
func (a *App) GroupChannelStats(id string) (map[string]ChannelStat, error) {
	return nil, errors.New("not built")
}
