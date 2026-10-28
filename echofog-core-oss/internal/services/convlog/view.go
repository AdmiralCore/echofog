package convlog

import (
	"encoding/json"
	"sort"
	"strings"
)

// Reg is one last-writer-wins register: the operation with the greatest (L, A, S) wins, the same
// on every device whatever order the operations arrived in.
type Reg struct {
	L    uint64 `json:"l"`            // Lamport clock of the operation
	A    string `json:"a"`            // its author (peer id hex)
	S    uint64 `json:"s"`            // its author's sequence number
	TS   int64  `json:"t"`            // its author's clock, ms (display)
	On   bool   `json:"on,omitempty"` // reactions and pins: set or cleared
	Text string `json:"x,omitempty"`  // edits: the new text
	Mod  bool   `json:"m,omitempty"`  // deletes: by a moderator rather than the author
}

func (r Reg) after(o Reg) bool {
	if r.L != o.L {
		return r.L > o.L
	}
	if r.A != o.A {
		return r.A > o.A
	}
	return r.S > o.S
}

// evRef points at an event in the log: author's peer id hex and sequence number.
type evRef struct {
	A string `json:"a"`
	S uint64 `json:"s"`
}

// View is the state of one message: the message itself once it has arrived, merged with every
// operation on it. Operations that come before the message are kept on a stub (Present false).
type View struct {
	ID      string `json:"id"`
	Present bool   `json:"present"`
	Legacy  bool   `json:"legacy,omitempty"` // imported from the old storage, not synced

	Author   string `json:"author,omitempty"` // peer id hex
	Seq      uint64 `json:"seq,omitempty"`
	Lamport  uint64 `json:"lamport,omitempty"`
	TS       int64  `json:"ts,omitempty"` // ms
	Text     string `json:"text,omitempty"`
	Type     uint32 `json:"type,omitempty"`
	Metadata string `json:"meta,omitempty"`
	ReplyTo  string `json:"reply_to,omitempty"`
	Thread   string `json:"thread,omitempty"`
	Channel  string `json:"channel,omitempty"`  // a group's channel ("" the main one)
	Withheld bool   `json:"withheld,omitempty"` // arrived without its body and no delete explains it

	// Edits keeps each editor's latest edit; only the author's counts (see Render), but the author
	// is not known while this is a stub.
	Edits map[string]Reg `json:"edits,omitempty"`
	// EditRefs are the edit events on this message, to strip when it is deleted.
	EditRefs []evRef `json:"edit_refs,omitempty"`
	// Deletes keeps who deleted the message.
	Deletes map[string]Reg `json:"deletes,omitempty"`
	// Reacts: emoji -> reactor (peer id hex) -> state.
	Reacts map[string]map[string]Reg `json:"reacts,omitempty"`
	// Pin is the conversation-wide pin state of this message.
	Pin *Reg `json:"pin,omitempty"`

	Replies     int   `json:"replies,omitempty"`       // messages in its thread
	LastReplyTS int64 `json:"last_reply_ts,omitempty"` // ms
	// ThreadTitle names the thread under the message (last writer wins).
	ThreadTitle *Reg `json:"thread_title,omitempty"`
	Stripped    bool  `json:"stripped,omitempty"`      // its event no longer carries the body
}

const maxEditRefs = 64

// deleted reports whether the message is gone for everyone: deleted by its author, or by a
// moderator.
func (v *View) deleted() (bool, string) {
	if !v.Present {
		return false, ""
	}
	if r, ok := v.Deletes[v.Author]; ok {
		return true, r.A
	}
	best := ""
	var bestReg Reg
	for who, r := range v.Deletes {
		if r.Mod && (best == "" || r.after(bestReg)) {
			best, bestReg = who, r
		}
	}
	return best != "", best
}

// edit returns the author's latest edit.
func (v *View) edit() (Reg, bool) {
	if !v.Present {
		return Reg{}, false
	}
	r, ok := v.Edits[v.Author]
	return r, ok
}

func (v *View) applyEdit(r Reg, ref evRef) {
	if v.Edits == nil {
		v.Edits = map[string]Reg{}
	}
	if cur, ok := v.Edits[r.A]; !ok || r.after(cur) {
		v.Edits[r.A] = r
	}
	if len(v.EditRefs) < maxEditRefs {
		v.EditRefs = append(v.EditRefs, ref)
	}
}

func (v *View) applyDelete(r Reg) {
	if v.Deletes == nil {
		v.Deletes = map[string]Reg{}
	}
	if cur, ok := v.Deletes[r.A]; !ok || r.after(cur) {
		v.Deletes[r.A] = r
	}
}

func (v *View) applyReact(emoji string, r Reg) {
	if v.Reacts == nil {
		v.Reacts = map[string]map[string]Reg{}
	}
	m := v.Reacts[emoji]
	if m == nil {
		m = map[string]Reg{}
		v.Reacts[emoji] = m
	}
	if cur, ok := m[r.A]; !ok || r.after(cur) {
		m[r.A] = r
	}
}

// applyPin reports whether the pin state changed.
func (v *View) applyPin(r Reg) bool {
	if v.Pin == nil || r.after(*v.Pin) {
		changed := v.Pin == nil || v.Pin.On != r.On
		v.Pin = &r
		return changed
	}
	return false
}

// Reaction is one emoji on a message and who set it.
type Reaction struct {
	Emoji string   `json:"emoji"`
	Count int      `json:"count"`
	By    []string `json:"by"` // peer ids hex
	Mine  bool     `json:"mine"`
}

// Message is what the app shows for one message: the view with every operation resolved.
type Message struct {
	ID          string     `json:"id"`
	Conv        string     `json:"conv"`
	Author      string     `json:"author"`
	Mine        bool       `json:"mine"`
	TS          int64      `json:"ts"` // ms
	Lamport     uint64     `json:"lamport"`
	Type        uint32     `json:"type"`
	Text        string     `json:"text"`
	Metadata    string     `json:"metadata,omitempty"`
	ReplyTo     string     `json:"replyTo,omitempty"`
	Thread      string     `json:"thread,omitempty"`
	Channel     string     `json:"channel,omitempty"`
	Edited      bool       `json:"edited"`
	EditedAt    int64      `json:"editedAt,omitempty"`
	Deleted     bool       `json:"deleted"`
	DeletedBy   string     `json:"deletedBy,omitempty"`
	Withheld    bool       `json:"withheld,omitempty"`
	Pinned      bool       `json:"pinned"`
	PinnedBy    string     `json:"pinnedBy,omitempty"`
	PinnedAt    int64      `json:"pinnedAt,omitempty"`
	Reactions   []Reaction `json:"reactions"`
	Replies     int        `json:"replies"`
	LastReplyAt int64      `json:"lastReplyAt,omitempty"`
	ThreadTitle string     `json:"threadTitle,omitempty"`
	Legacy      bool       `json:"legacy,omitempty"`
	// State is the delivery state of my own message (keys, offline, relayed, sent, delivered),
	// filled in by the caller from the outbox.
	State string `json:"state,omitempty"`

	// Kind is what the message is: "text", "file", "call" (an invitation to a call) or "system"
	// (a line the app wrote itself).
	Kind string `json:"kind"`
	// File is the file a file message offers (nil for any other kind, and once deleted).
	File *FileRef `json:"file,omitempty"`
	// Can is what I may do with the message, filled in by the caller.
	Can MsgCan `json:"can"`
}

// FileRef is a file a message offers: the transfer to fetch it by, its name, size and type.
type FileRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Mime string `json:"mime,omitempty"`
}

// MsgCan is what I may do with one message.
type MsgCan struct {
	Edit   bool `json:"edit"`
	Delete bool `json:"delete"`
	Pin    bool `json:"pin"`
	React  bool `json:"react"`
	Reply  bool `json:"reply"`
}

// Message types the view tells apart (as messenger.PersonalMessageTypeProto).
const msgTypeFile uint32 = 4

// CallInvitePrefix starts the text of an invitation to a call.
const CallInvitePrefix = "fogcall://"

// fileOf reads the file a message offers from its metadata: a group file ({"group_file": ...}) or
// a direct one ({"file_name", "file_size", "mime_type", "custom": {"fid"}}).
func fileOf(typ uint32, text, metadata string) *FileRef {
	var meta struct {
		GroupFile *struct {
			ID   string      `json:"id"`
			Name string      `json:"name"`
			Size json.Number `json:"size"`
			Mime string      `json:"mime"`
		} `json:"group_file"`
		FileName string      `json:"file_name"`
		FileSize json.Number `json:"file_size"`
		MimeType string      `json:"mime_type"`
		Custom   struct {
			FID string `json:"fid"`
		} `json:"custom"`
	}
	if metadata != "" {
		_ = json.Unmarshal([]byte(metadata), &meta)
	}
	if g := meta.GroupFile; g != nil {
		n, _ := g.Size.Int64()
		return &FileRef{ID: g.ID, Name: g.Name, Size: n, Mime: g.Mime}
	}
	if typ != msgTypeFile {
		return nil
	}
	name := meta.FileName
	if name == "" {
		name = text
	}
	n, _ := meta.FileSize.Int64()
	return &FileRef{ID: meta.Custom.FID, Name: name, Size: n, Mime: meta.MimeType}
}

// Render resolves the view for display; self is my peer id hex.
func (v *View) Render(conv, self string) Message {
	m := Message{
		ID: v.ID, Conv: conv, Author: v.Author, Mine: v.Author == self && self != "",
		TS: v.TS, Lamport: v.Lamport, Type: v.Type, Text: v.Text, Metadata: v.Metadata,
		ReplyTo: v.ReplyTo, Thread: v.Thread, Channel: v.Channel, Replies: v.Replies, LastReplyAt: v.LastReplyTS,
		Legacy: v.Legacy, Withheld: v.Withheld, Reactions: []Reaction{},
	}
	if v.ThreadTitle != nil {
		m.ThreadTitle = v.ThreadTitle.Text
	}
	m.Kind = "text"
	switch {
	case v.Type == MsgTypeSystem:
		m.Kind = "system"
	case strings.HasPrefix(v.Text, CallInvitePrefix):
		m.Kind = "call"
	default:
		if f := fileOf(v.Type, v.Text, v.Metadata); f != nil {
			m.Kind, m.File = "file", f
		}
	}
	if del, by := v.deleted(); del {
		m.Deleted, m.DeletedBy = true, by
		m.Text, m.Metadata, m.Withheld, m.File = "", "", false, nil
	} else if e, ok := v.edit(); ok {
		m.Text, m.Edited, m.EditedAt = e.Text, true, e.TS
	}
	if v.Pin != nil && v.Pin.On && !m.Deleted {
		m.Pinned, m.PinnedBy, m.PinnedAt = true, v.Pin.A, v.Pin.TS
	}
	if !m.Deleted {
		emojis := make([]string, 0, len(v.Reacts))
		for e := range v.Reacts {
			emojis = append(emojis, e)
		}
		sort.Strings(emojis)
		for _, e := range emojis {
			r := Reaction{Emoji: e}
			for who, reg := range v.Reacts[e] {
				if reg.On {
					r.By = append(r.By, who)
					if who == self {
						r.Mine = true
					}
				}
			}
			if len(r.By) == 0 {
				continue
			}
			sort.Strings(r.By)
			r.Count = len(r.By)
			m.Reactions = append(m.Reactions, r)
		}
	}
	return m
}
