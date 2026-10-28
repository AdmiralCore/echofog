package node

import (
	"encoding/base64"
	"errors"
	"regexp"
	"strings"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// What a chat holds besides its text: photos and videos, other files, and the links its messages
// carry, newest first. And the profile I keep of a contact: the name and picture I gave it.

// ChatLink is a link a message carries.
type ChatLink struct {
	URL       string `json:"url"`
	MessageID string `json:"messageId"`
	Author    string `json:"author"`
	Mine      bool   `json:"mine"`
	TS        int64  `json:"ts"` // ms
}

// ChatAttachments is what ChatAttachments returns.
type ChatAttachments struct {
	Media []ChatMessage `json:"media"` // photos and videos
	Files []ChatMessage `json:"files"` // every other file
	Links []ChatLink    `json:"links"`
}

const attachmentScan = 3000

var linkRe = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"'` + "`" + `]+`)

func isMedia(mime, name string) bool {
	if strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "video/") {
		return true
	}
	n := strings.ToLower(name)
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".mp4", ".mov", ".webm", ".mkv"} {
		if strings.HasSuffix(n, ext) {
			return true
		}
	}
	return false
}

// ChatAttachments lists a chat's media, files and links (every channel of a group), newest first.
func (n *NodeApi) ChatAttachments(ref ChatRef) (ChatAttachments, error) {
	f, c, err := n.chat(ref)
	if err != nil {
		return ChatAttachments{}, err
	}
	channels := []string{""}
	if c.Kind == convlog.KindGroup {
		for _, ch := range f.log.Channels(c) {
			if !ch.Voice {
				channels = append(channels, ch.ID)
			}
		}
	}
	out := ChatAttachments{Media: []ChatMessage{}, Files: []ChatMessage{}, Links: []ChatLink{}}
	seen := 0
	for _, ch := range channels {
		cursor := ""
		for seen < attachmentScan {
			page, next, err := f.log.HistoryIn(c, ch, cursor, 200)
			if err != nil {
				break
			}
			for _, m := range page {
				seen++
				if m.Deleted {
					continue
				}
				if m.File != nil {
					if isMedia(m.File.Mime, m.File.Name) {
						out.Media = append(out.Media, m)
					} else {
						out.Files = append(out.Files, m)
					}
				}
				for _, u := range linkRe.FindAllString(m.Text, 20) {
					out.Links = append(out.Links, ChatLink{URL: strings.TrimRight(u, ".,;:!?)]}"), MessageID: m.ID, Author: m.Author, Mine: m.Mine, TS: m.TS})
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	return out, nil
}

// maxAvatarBytes bounds a contact picture (it is kept on this device only).
const maxAvatarBytes = 512 << 10

// SetContactProfile renames a contact and sets the picture I see for it ("" removes it). Both are
// mine alone: the contact is not told.
func (n *NodeApi) SetContactProfile(peerID, alias, avatarBase64 string) error {
	cs, err := n.contactsReady()
	if err != nil {
		return err
	}
	if _, err := types.PeerIDFromString(peerID); err != nil {
		return errors.New("bad peer id")
	}
	alias = strings.TrimSpace(alias)
	if len(alias) > 64 {
		return errors.New("a name is at most 64 characters")
	}
	if avatarBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(avatarBase64)
		if err != nil || len(raw) == 0 || len(raw) > maxAvatarBytes {
			return errors.New("a picture must be an image of at most 512 KB")
		}
	}
	return cs.SetProfile(peerID, alias, avatarBase64)
}

// contactOf is a contact as stored (its name and picture), false for somebody not a contact.
func (n *NodeApi) contactOf(peerID string) (alias, avatar, pub string, ok bool) {
	if n.node == nil || n.node.Contact == nil {
		return "", "", "", false
	}
	list, err := n.node.Contact.GiveMyContacts()
	if err != nil {
		return "", "", "", false
	}
	for _, c := range list {
		if c.PeerID == peerID {
			return c.Alias, c.Avatar, c.PubKey, true
		}
	}
	return "", "", "", false
}
