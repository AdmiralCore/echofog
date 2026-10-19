package contacts

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/dto"
)

// Blocked people and message requests. A blocked peer is ignored everywhere: messages, calls,
// files, group invitations, its messages in groups. A request is a stranger who wrote first: the
// conversation is kept, but the stranger is not a contact (cannot call, send files or count as
// one of ours) until the user accepts. Both lists are sealed, and keys are blinded like contacts.

const (
	blockedPrefix = "blocked:"
	requestPrefix = "msgreq:"
)

// Blocked is one blocked peer.
type Blocked struct {
	PeerID string `json:"peer_id"`
	Alias  string `json:"alias,omitempty"` // the name they had when blocked, to recognise them in the list
	At     int64  `json:"at"`
}

// Request is a stranger who sent a message.
type Request struct {
	PeerID string `json:"peer_id"`
	PubKey string `json:"pub_key,omitempty"`
	First  int64  `json:"first"`
	Last   int64  `json:"last"`
	Count  int    `json:"count"`
}

var ErrBadPeer = errors.New("not a peer ID")

func normPeer(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) != 64 {
		return "", ErrBadPeer
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return "", ErrBadPeer
		}
	}
	return id, nil
}

func (c *ContactService) blockedKey(id string) []byte {
	return []byte(blockedPrefix + storage.BlindID(c.storage, id))
}

func (c *ContactService) requestKey(id string) []byte {
	return []byte(requestPrefix + storage.BlindID(c.storage, id))
}

// Block blocks a peer. It also drops a pending request from it; a contact stays a contact (the
// block wins) so that unblocking brings everything back.
func (c *ContactService) Block(peerID, alias string) error {
	id, err := normPeer(peerID)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(Blocked{PeerID: id, Alias: alias, At: time.Now().Unix()})
	if err := c.storage.Set(c.blockedKey(id), raw); err != nil {
		return err
	}
	_ = c.storage.Delete(c.requestKey(id))
	return nil
}

// Unblock lifts a block.
func (c *ContactService) Unblock(peerID string) error {
	id, err := normPeer(peerID)
	if err != nil {
		return err
	}
	return c.storage.Delete(c.blockedKey(id))
}

// IsBlocked reports whether a peer is blocked.
func (c *ContactService) IsBlocked(peerID string) bool {
	id, err := normPeer(peerID)
	if err != nil {
		return false
	}
	return c.storage.Exists(c.blockedKey(id))
}

// BlockedList returns the blocked peers, the latest first.
func (c *ContactService) BlockedList() ([]Blocked, error) {
	vals, err := c.storage.FindValues([]byte(blockedPrefix), 0)
	if err != nil {
		return nil, err
	}
	out := make([]Blocked, 0, len(vals))
	for _, v := range vals {
		var b Blocked
		if json.Unmarshal(v.Value, &b) == nil && b.PeerID != "" {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out, nil
}

// NoteRequest records a message from a stranger (one that is neither a contact nor blocked).
func (c *ContactService) NoteRequest(peerID, pubKey string) error {
	id, err := normPeer(peerID)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	r := Request{PeerID: id, First: now}
	if raw, err := c.storage.Get(c.requestKey(id)); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &r)
	}
	if pubKey != "" {
		r.PubKey = pubKey
	}
	r.Last = now
	r.Count++
	raw, _ := json.Marshal(r)
	return c.storage.Set(c.requestKey(id), raw)
}

// IsRequest reports whether a peer has a pending request.
func (c *ContactService) IsRequest(peerID string) bool {
	id, err := normPeer(peerID)
	if err != nil {
		return false
	}
	return c.storage.Exists(c.requestKey(id))
}

// Requests returns pending requests, the latest message first.
func (c *ContactService) Requests() ([]Request, error) {
	vals, err := c.storage.FindValues([]byte(requestPrefix), 0)
	if err != nil {
		return nil, err
	}
	out := make([]Request, 0, len(vals))
	for _, v := range vals {
		var r Request
		if json.Unmarshal(v.Value, &r) == nil && r.PeerID != "" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last > out[j].Last })
	return out, nil
}

// AcceptRequest turns a request into a contact named alias.
func (c *ContactService) AcceptRequest(peerID, alias string) error {
	id, err := normPeer(peerID)
	if err != nil {
		return err
	}
	var r Request
	raw, err := c.storage.Get(c.requestKey(id))
	if err != nil || len(raw) == 0 || json.Unmarshal(raw, &r) != nil {
		return errors.New("no request from this peer")
	}
	if strings.TrimSpace(alias) == "" {
		alias = "Unknown User"
	}
	if err := c.NewContact(dto.ContactDTO{PeerID: id, PubKey: r.PubKey, Alias: alias, LastSeen: r.Last}); err != nil {
		return err
	}
	return c.storage.Delete(c.requestKey(id))
}

// DismissRequest forgets a request without blocking (the conversation stays where it is).
func (c *ContactService) DismissRequest(peerID string) error {
	id, err := normPeer(peerID)
	if err != nil {
		return err
	}
	return c.storage.Delete(c.requestKey(id))
}
