package node

import (
	"errors"
	"sync/atomic"

	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Who may reach the user directly: write first, call. Blocked peers are refused everything, and
// in groups their messages and calls are ignored.
const (
	WhoEveryone = "everyone"
	WhoContacts = "contacts"
	WhoNobody   = "nobody"
)

// Privacy is the user's choice of who may write and call.
type Privacy struct {
	// Messages: WhoEveryone (strangers land in message requests, the default) or WhoContacts
	// (strangers are dropped).
	Messages string `json:"messages"`
	// Calls: WhoContacts (the default), WhoEveryone or WhoNobody. Group calls ring for members
	// either way, unless the caller is blocked.
	Calls string `json:"calls"`
}

// DefaultPrivacy: strangers may write (as requests), only contacts may call.
var DefaultPrivacy = Privacy{Messages: WhoEveryone, Calls: WhoContacts}

var ErrBadPrivacy = errors.New("messages: everyone or contacts; calls: everyone, contacts or nobody")

// Valid reports whether p names known choices.
func (p Privacy) Valid() bool {
	okMsg := p.Messages == WhoEveryone || p.Messages == WhoContacts
	okCall := p.Calls == WhoEveryone || p.Calls == WhoContacts || p.Calls == WhoNobody
	return okMsg && okCall
}

type privacyHolder struct{ p atomic.Pointer[Privacy] }

// SetPrivacy applies the user's choice at once.
func (n *Node) SetPrivacy(p Privacy) error {
	if !p.Valid() {
		return ErrBadPrivacy
	}
	n.privacy.p.Store(&p)
	return nil
}

// GetPrivacy returns the choice in effect.
func (n *Node) GetPrivacy() Privacy {
	if p := n.privacy.p.Load(); p != nil {
		return *p
	}
	return DefaultPrivacy
}

// IsBlocked reports whether the user blocked peer.
func (n *Node) IsBlocked(peer types.PeerID) bool {
	return n.Contact != nil && n.Contact.IsBlocked(peer.String())
}

// IsContact reports whether peer is a contact the user did not block.
func (n *Node) IsContact(peer types.PeerID) bool {
	return n.Contact != nil && n.Contact.Exists(peer.String()) && !n.Contact.IsBlocked(peer.String())
}

// messageVerdict decides what happens to a direct message from peer.
func (n *Node) messageVerdict(peer types.PeerID) messenger.Verdict {
	switch {
	case n.IsBlocked(peer):
		return messenger.Drop
	case n.Contact != nil && n.Contact.Exists(peer.String()):
		return messenger.Accept
	case n.GetPrivacy().Messages == WhoContacts:
		return messenger.Drop
	default:
		return messenger.AsRequest
	}
}

// CanRing reports whether a direct call from peer may ring.
func (n *Node) CanRing(peer types.PeerID) bool {
	if n.IsBlocked(peer) {
		return false
	}
	switch n.GetPrivacy().Calls {
	case WhoEveryone:
		return true
	case WhoNobody:
		return false
	default:
		return n.IsContact(peer)
	}
}

// AcceptsFilesFrom reports whether a direct file offer from peer is shown: contacts only, as files
// from strangers are an easy way to push something onto a disk.
func (n *Node) AcceptsFilesFrom(peer types.PeerID) bool { return n.IsContact(peer) }
