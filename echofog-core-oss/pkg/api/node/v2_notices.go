package node

import (
	"errors"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/app"
)

// UpdateNotice is a release the UI offers (T11.5). Nothing is downloaded or installed.
type UpdateNotice struct {
	Version    string   `json:"version"`
	NotesURL   string   `json:"notesUrl,omitempty"`
	Important  bool     `json:"important"`
	Components []string `json:"components,omitempty"`
}

// ErrorScreenInfo is a full-screen error the UI shows for a code.
type ErrorScreenInfo struct {
	Code  string `json:"code"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// v2Notices keeps the app layer the notices come from and the UI's handler of them.
type v2Notices struct {
	mu     sync.Mutex
	app    v2App
	cancel func()
	fn     func(kind, text string, update *UpdateNotice)
}

// attach subscribes to the app's notices, dropping the subscription of an earlier app.
func (v *v2Notices) attach(a v2App) {
	v.mu.Lock()
	if v.cancel != nil {
		v.cancel()
	}
	v.app = a
	v.mu.Unlock()
	cancel := a.Subscribe(v.deliver)
	v.mu.Lock()
	v.cancel = cancel
	v.mu.Unlock()
}

func (v *v2Notices) deliver(n app.Notice) {
	v.mu.Lock()
	fn := v.fn
	v.mu.Unlock()
	if fn != nil {
		fn(string(n.Kind), n.Text, updateNotice(n.Update))
	}
}

func updateNotice(u *app.UpdateView) *UpdateNotice {
	if u == nil {
		return nil
	}
	return &UpdateNotice{Version: u.Version, NotesURL: u.NotesURL, Important: u.Important, Components: append([]string(nil), u.Components...)}
}

// V2Update is the newest release the signed manifest offers, or nil.
func (n *NodeApi) V2Update() *UpdateNotice {
	n.v2n.mu.Lock()
	a := n.v2n.app
	n.v2n.mu.Unlock()
	if a == nil {
		return nil
	}
	return updateNotice(a.LastUpdate())
}

// OnV2Notice registers the UI's handler of runtime notices (kind, Russian text, the update if
// the notice is one). Call it before or after AttachV2; one handler is kept. The handler runs on
// the runtime's goroutine and must not block.
func (n *NodeApi) OnV2Notice(fn func(kind, text string, update *UpdateNotice)) {
	n.v2n.mu.Lock()
	n.v2n.fn = fn
	n.v2n.mu.Unlock()
}

// ErrorScreenOf says which full-screen error an error of this facade has.
func ErrorScreenOf(err error) (ErrorScreenInfo, bool) {
	var e *v2UserError
	if !errors.As(err, &e) || e.screen == nil {
		return ErrorScreenInfo{}, false
	}
	return ErrorScreenInfo{Code: e.screen.Code, Title: e.screen.Title, Text: e.screen.Text}, true
}
