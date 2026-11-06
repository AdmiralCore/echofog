package app

import (
	"errors"

	"github.com/echofog/echofog-core/internal/v2/node"
)

// NoticeKind names a notice the UI shows (T11.5): what the runtime reports, in the app's words.
type NoticeKind string

const (
	// NoticeUpdate: a newer release exists (a signed manifest says so). Nothing is downloaded.
	NoticeUpdate NoticeKind = "update"
	// NoticeNoDomesticEntry: the country policy has no entry node (T6.4).
	NoticeNoDomesticEntry NoticeKind = "no-domestic-entry"
	// NoticeReleaseUnavailable: the stored release data is damaged, so updates are not checked.
	NoticeReleaseUnavailable NoticeKind = "release-unavailable"
	// NoticeNoCover: this node has no camouflaged listener (D37).
	NoticeNoCover NoticeKind = "no-cover"
	// NoticeClockSkew: the device clock differs from the network's (T3.8-fix2).
	NoticeClockSkew NoticeKind = "clock-skew"
)

// ErrFreeQuotaUsed is what the exit refusal "free volume for today is used" maps to (T10.3).
var ErrFreeQuotaUsed = errors.New("app: free quota used")

// UpdateView is a release the UI offers. The manifest has no "security" flag, so Important means
// the build is below min_supported of a component: peers will stop accepting it.
type UpdateView struct {
	Version    string
	NotesURL   string // https only, "" when the manifest had none
	Important  bool
	Components []string
}

// Notice is one thing the UI is told. Text is Russian in a neutral form; Update is set for
// NoticeUpdate only.
type Notice struct {
	Kind   NoticeKind
	Text   string
	Update *UpdateView
}

// ErrorScreen is a full-screen error of the UI: a stable Code (the UI picks its English text by
// it) and the Russian Title and Text.
type ErrorScreen struct {
	Code  string
	Title string
	Text  string
}

// Sink returns the sink to give node.Config.Events: it turns runtime events into notices.
func (a *App) Sink() node.EventSink { return node.EventFunc(func(node.Event) {}) }

// Subscribe registers fn for notices; the returned function removes it.
func (a *App) Subscribe(fn func(Notice)) (cancel func()) { return func() {} }

// LastUpdate is the newest update notice seen, or nil.
func (a *App) LastUpdate() *UpdateView { return nil }

// Screen picks the error screen for an error, if one exists.
func Screen(err error) (ErrorScreen, bool) { return ErrorScreen{}, false }

// ScreenByCode returns the error screen with that code.
func ScreenByCode(code string) (ErrorScreen, bool) { return ErrorScreen{}, false }
