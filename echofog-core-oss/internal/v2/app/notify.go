package app

import (
	"errors"

	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Notification modes of a channel as the UI names them.
const (
	NotifyAll      = "all"
	NotifyMentions = "mentions"
	NotifyNone     = "none"
)

var errNotBuilt = errors.New("app: not built")

// ChatNotify tells what a chat's channel tells the user about (stub).
func (a *App) ChatNotify(ref ChatRef) (string, error) { return "", errNotBuilt }

// SetChatNotify sets it (stub).
func (a *App) SetChatNotify(ref ChatRef, mode string) error { return errNotBuilt }

// ChatFollow follows or leaves the thread under root (stub).
func (a *App) ChatFollow(ref ChatRef, root string, on bool) error { return errNotBuilt }

// ChatFollowing lists the thread roots the user follows in a chat (stub).
func (a *App) ChatFollowing(ref ChatRef) ([]string, error) { return nil, errNotBuilt }

// NewVoiceRooms turns the live roster into the app's VoiceRooms (stub).
func NewVoiceRooms(v *convsync.VoiceRoster, b *GroupBook) VoiceRooms { return noRooms{} }

type noRooms struct{}

func (noRooms) Roster(group, channel string) []VoiceMember { return nil }

var _ = store.KV(nil)
