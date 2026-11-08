package app

import (
	"encoding/json"
	"errors"
	"sort"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/v2/store"
)

// Saved messages ("Favorites"): bookmarks kept on this device only, in the app's own store. They are
// never sent anywhere and never enter a conversation log (T11.3e).

var (
	// ErrBadSaved reports a bookmark without a chat or a message, or with fields beyond their bounds.
	ErrBadSaved = errors.New("app: bad saved message")
	// ErrSavedFull reports a new bookmark beyond the limit.
	ErrSavedFull = errors.New("app: too many saved messages")
)

const (
	// MaxSaved bounds the bookmarks.
	MaxSaved = 5000
	// maxSavedText bounds one bookmarked text; a longer one is cut at a rune.
	maxSavedText = 64 << 10
	// maxSavedID bounds the chat and the message id of a bookmark and its other short fields.
	maxSavedID    = 256
	maxSavedField = 512
	savedPrefix   = "sv/"
)

// SavedMessage is a copy of a message the user bookmarked.
type SavedMessage struct {
	ID, ChatID, ChatName, MsgID string
	Channel, Thread, Sender     string
	Mine                        bool
	Text, FileName              string
	FileSize                    int64
	TS, SavedAt                 int64 // unix ms
}

func okShort(s string, max int) bool { return len(s) <= max && utf8.ValidString(s) }

func (m *SavedMessage) valid() bool {
	return m.ChatID != "" && m.MsgID != "" && okShort(m.ChatID, maxSavedID) && okShort(m.MsgID, maxSavedID) &&
		okShort(m.Channel, maxSavedID) && okShort(m.Thread, maxSavedID) && okShort(m.Sender, maxSavedID) &&
		okShort(m.ChatName, maxSavedField) && okShort(m.FileName, maxSavedField) && utf8.ValidString(m.Text) && m.FileSize >= 0
}

// SaveMessage bookmarks a copy of a message; saving it again updates the copy.
func (a *App) SaveMessage(m SavedMessage) (SavedMessage, error) {
	if !m.valid() {
		return SavedMessage{}, ErrBadSaved
	}
	if len(m.Text) > maxSavedText {
		cut := maxSavedText
		for cut > 0 && !utf8.RuneStart(m.Text[cut]) {
			cut--
		}
		m.Text = m.Text[:cut]
	}
	m.ID = m.ChatID + "/" + m.MsgID
	m.SavedAt = a.cfg.Clock.Now().UnixMilli()
	b, err := json.Marshal(m)
	if err != nil {
		return SavedMessage{}, err
	}
	a.svmu.Lock()
	defer a.svmu.Unlock()
	key := []byte(savedPrefix + m.ID)
	have, err := a.cfg.Store.Has(key)
	if err != nil {
		return SavedMessage{}, err
	}
	if !have {
		if err := a.countSaved(); err != nil {
			return SavedMessage{}, err
		}
		if a.svn >= MaxSaved {
			return SavedMessage{}, ErrSavedFull
		}
	}
	if err := a.cfg.Store.Put(key, b); err != nil {
		return SavedMessage{}, err
	}
	if !have && a.svCounted {
		a.svn++
	}
	return m, nil
}

// UnsaveMessage removes a bookmark; a missing one is no error.
func (a *App) UnsaveMessage(id string) error {
	if id == "" || len(id) > 2*maxSavedID+1 {
		return nil
	}
	a.svmu.Lock()
	defer a.svmu.Unlock()
	key := []byte(savedPrefix + id)
	if have, _ := a.cfg.Store.Has(key); have && a.svCounted && a.svn > 0 {
		a.svn--
	}
	return a.cfg.Store.Delete(key)
}

// countSaved counts the bookmarks once; Save and Unsave keep the count after that. Call with svmu held.
func (a *App) countSaved() error {
	if a.svCounted {
		return nil
	}
	n := 0
	err := a.eachSaved(func(store.Entry) bool { n++; return true })
	if err != nil {
		return err
	}
	a.svn, a.svCounted = n, true
	return nil
}

// eachSaved walks the bookmarks page by page (a scan returns a page at most) until fn says stop.
func (a *App) eachSaved(fn func(store.Entry) bool) error {
	prefix := []byte(savedPrefix)
	var after []byte
	for {
		es, err := a.cfg.Store.ScanFrom(prefix, after, store.MaxScanLimit)
		if err != nil {
			return err
		}
		for _, e := range es {
			if !fn(e) {
				return nil
			}
		}
		if len(es) < store.MaxScanLimit {
			return nil
		}
		after = es[len(es)-1].Key
	}
}

// ListSavedMessages lists the bookmarks, the latest saved first.
func (a *App) ListSavedMessages() ([]SavedMessage, error) {
	var out []SavedMessage
	err := a.eachSaved(func(e store.Entry) bool {
		var m SavedMessage
		if json.Unmarshal(e.Value, &m) == nil && m.ID != "" {
			out = append(out, m)
		}
		return len(out) < MaxSaved
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []SavedMessage{}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SavedAt > out[j].SavedAt })
	return out, nil
}
