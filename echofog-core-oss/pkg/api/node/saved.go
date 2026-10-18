package node

import (
	"encoding/json"
	"errors"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/echofog/echofog-core/pkg/dto"
)

// Saved messages live under "bm:" (storage.SealedPrefixes encrypts them like the chats themselves).
const savedPrefix = "bm:"

// maxSavedText bounds one saved message; longer text is cut.
const maxSavedText = 64 << 10

// SaveMessage bookmarks a copy of a message. Saving it again updates the copy.
func (n *NodeApi) SaveMessage(m dto.SavedMessageDTO) (dto.SavedMessageDTO, error) {
	if n.node == nil || n.node.Storage == nil {
		return m, errors.New("node not started")
	}
	if m.ChatID == "" || m.MsgID == "" {
		return m, errors.New("a saved message needs its chat and message")
	}
	if len(m.Text) > maxSavedText {
		cut := maxSavedText
		for cut > 0 && !utf8.RuneStart(m.Text[cut]) {
			cut--
		}
		m.Text = m.Text[:cut]
	}
	m.ID = m.ChatID + "/" + m.MsgID
	m.SavedAt = time.Now().UnixMilli()
	b, err := json.Marshal(m)
	if err != nil {
		return m, err
	}
	return m, n.node.Storage.Set([]byte(savedPrefix+m.ID), b)
}

// UnsaveMessage removes a bookmark.
func (n *NodeApi) UnsaveMessage(id string) error {
	if n.node == nil || n.node.Storage == nil {
		return errors.New("node not started")
	}
	return n.node.Storage.Delete([]byte(savedPrefix + id))
}

// ListSavedMessages is every bookmark, the latest saved first.
func (n *NodeApi) ListSavedMessages() ([]dto.SavedMessageDTO, error) {
	if n.node == nil || n.node.Storage == nil {
		return nil, errors.New("node not started")
	}
	entries, err := n.node.Storage.FindValues([]byte(savedPrefix), 0)
	if err != nil {
		return nil, err
	}
	out := make([]dto.SavedMessageDTO, 0, len(entries))
	for _, e := range entries {
		var m dto.SavedMessageDTO
		if json.Unmarshal(e.Value, &m) == nil && m.ID != "" {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt > out[j].SavedAt })
	return out, nil
}
