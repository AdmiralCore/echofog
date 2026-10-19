package node

import (
	"errors"
	"strings"

	"github.com/echofog/echofog-core/internal/services/contacts"
	"github.com/echofog/echofog-core/pkg/api/types"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// PrivacyDTO is who may write and call: messages "everyone" | "contacts", calls "everyone" |
// "contacts" | "nobody".
type PrivacyDTO = coreNode.Privacy

// BlockedDTO is one blocked peer; MessageRequestDTO is a stranger who wrote first.
type (
	BlockedDTO        = contacts.Blocked
	MessageRequestDTO = contacts.Request
)

// GetPrivacy returns who may write and call.
func (n *NodeApi) GetPrivacy() PrivacyDTO {
	if n.node == nil {
		return coreNode.DefaultPrivacy
	}
	return n.node.GetPrivacy()
}

// SetPrivacy saves and applies who may write and call.
func (n *NodeApi) SetPrivacy(p PrivacyDTO) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node is not running")
	}
	if !p.Valid() {
		return coreNode.ErrBadPrivacy
	}
	settingsMu.Lock()
	err := saveSettingsLocked(n.node.Cfg, func(ps *persistedSettings) { ps.Privacy = &p })
	settingsMu.Unlock()
	if err != nil {
		return err
	}
	return n.node.SetPrivacy(p)
}

func (n *NodeApi) contactsReady() (*contacts.ContactService, error) {
	if n.node == nil || n.node.Contact == nil {
		return nil, errors.New("node is not running")
	}
	return n.node.Contact, nil
}

// BlockPeer blocks a peer: its messages, calls, files and group invitations are ignored, and in
// shared groups its messages and calls stay hidden. It is not told.
func (n *NodeApi) BlockPeer(peerID string) error {
	c, err := n.contactsReady()
	if err != nil {
		return err
	}
	peerID = strings.ToLower(strings.TrimSpace(peerID))
	alias, _ := c.GetPeerAlias(peerID)
	if list, err := c.GiveMyContacts(); err == nil {
		for _, ct := range list {
			if strings.EqualFold(ct.PeerID, peerID) && ct.Alias != "" {
				alias = ct.Alias
			}
		}
	}
	return c.Block(peerID, alias)
}

// UnblockPeer lifts a block.
func (n *NodeApi) UnblockPeer(peerID string) error {
	c, err := n.contactsReady()
	if err != nil {
		return err
	}
	return c.Unblock(peerID)
}

// GetBlockedPeers lists the blocked peers, the latest first.
func (n *NodeApi) GetBlockedPeers() ([]contacts.Blocked, error) {
	c, err := n.contactsReady()
	if err != nil {
		return nil, err
	}
	list, err := c.BlockedList()
	if list == nil {
		list = []contacts.Blocked{}
	}
	return list, err
}

// GetMessageRequests lists strangers who wrote first, the latest first. Their messages are read
// with GetMessages like any chat.
func (n *NodeApi) GetMessageRequests() ([]contacts.Request, error) {
	c, err := n.contactsReady()
	if err != nil {
		return nil, err
	}
	list, err := c.Requests()
	if list == nil {
		list = []contacts.Request{}
	}
	return list, err
}

// AcceptMessageRequest makes the stranger a contact named alias.
func (n *NodeApi) AcceptMessageRequest(peerID, alias string) error {
	c, err := n.contactsReady()
	if err != nil {
		return err
	}
	return c.AcceptRequest(peerID, alias)
}

// DismissMessageRequest removes a request without blocking; the stranger can write again.
func (n *NodeApi) DismissMessageRequest(peerID string) error {
	c, err := n.contactsReady()
	if err != nil {
		return err
	}
	return c.DismissRequest(peerID)
}

// CanRing reports whether a direct call from peer may ring (blocked peers and, by default,
// strangers may not).
func (n *NodeApi) CanRing(peerID string) bool {
	id, err := types.PeerIDFromString(strings.ToLower(strings.TrimSpace(peerID)))
	if err != nil || n.node == nil {
		return false
	}
	return n.node.CanRing(id)
}
