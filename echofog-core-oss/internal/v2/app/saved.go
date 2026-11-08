package app

import (
	"errors"
)

// Saved messages ("Favorites"): bookmarks kept on this device only, in the app's own store. They are
// never sent anywhere and never enter a conversation log (T11.3e).

var (
	// ErrBadSaved reports a bookmark without a chat or a message, or with fields beyond their bounds.
	ErrBadSaved = errors.New("app: bad saved message")
	// ErrSavedFull reports a new bookmark beyond the limit.
	ErrSavedFull = errors.New("app: too many saved messages")
)

// MaxSaved bounds the bookmarks.
const MaxSaved = 5000

// SavedMessage is a copy of a message the user bookmarked.
type SavedMessage struct {
	ID, ChatID, ChatName, MsgID string
	Channel, Thread, Sender     string
	Mine                        bool
	Text, FileName              string
	FileSize                    int64
	TS, SavedAt                 int64 // unix ms
}

// SaveMessage bookmarks a copy of a message; saving it again updates the copy.
func (a *App) SaveMessage(m SavedMessage) (SavedMessage, error) { return m, errors.New("not built") }

// UnsaveMessage removes a bookmark; a missing one is no error.
func (a *App) UnsaveMessage(id string) error { return errors.New("not built") }

// ListSavedMessages lists the bookmarks, the latest saved first.
func (a *App) ListSavedMessages() ([]SavedMessage, error) { return nil, errors.New("not built") }
