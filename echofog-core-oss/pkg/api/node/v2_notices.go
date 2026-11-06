package node

import "github.com/echofog/echofog-core/internal/v2/app"

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

// V2Update is the newest release the signed manifest offers, or nil.
func (n *NodeApi) V2Update() *UpdateNotice { return nil }

// OnV2Notice registers the UI's handler of runtime notices (kind, Russian text, the update if
// the notice is one). Call it before or after AttachV2; one handler is kept.
func (n *NodeApi) OnV2Notice(fn func(kind, text string, update *UpdateNotice)) {}

// ErrorScreenOf says which full-screen error an error of this facade has.
func ErrorScreenOf(err error) (ErrorScreenInfo, bool) { return ErrorScreenInfo{}, false }

var _ = app.NoticeUpdate
