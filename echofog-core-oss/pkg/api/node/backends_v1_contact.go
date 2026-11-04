package node

import (
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/services/contacts"
	"github.com/echofog/echofog-core/pkg/dto"
)

// v1ContactBackend is the protocol 1 implementation of contactBackend: the code that used to sit
// in the NodeApi methods of this domain, moved here unchanged by T11.1b. The NodeApi methods below
// are one-line delegates.
type v1ContactBackend struct{ *NodeApi }

// contactDomain is the contacts backend of this facade, chosen the same way as netDomain.
func (n *NodeApi) contactDomain() contactBackend {
	if n.backends.contact != nil {
		return n.backends.contact
	}
	if n.node != nil && n.node.Cfg != nil && n.node.Cfg.ProtocolOrDefault() == config.ProtocolV2 {
		return v2PendingBackend{}
	}
	return &v1ContactBackend{n}
}

func (n *NodeApi) AcceptMessageRequest(peerID, alias string) error {
	return n.contactDomain().AcceptMessageRequest(peerID, alias)
}

func (n *NodeApi) BanContact(peerID string) error {
	return n.contactDomain().BanContact(peerID)
}

func (n *NodeApi) BlockPeer(peerID string) error {
	return n.contactDomain().BlockPeer(peerID)
}

func (n *NodeApi) CanRing(peerID string) bool {
	return n.contactDomain().CanRing(peerID)
}

func (n *NodeApi) DeleteContact(peerID string) error {
	return n.contactDomain().DeleteContact(peerID)
}

func (n *NodeApi) DismissMessageRequest(peerID string) error {
	return n.contactDomain().DismissMessageRequest(peerID)
}

func (n *NodeApi) GetBlockedPeers() ([]contacts.Blocked, error) {
	return n.contactDomain().GetBlockedPeers()
}

func (n *NodeApi) GetContacts() ([]dto.ContactDTO, error) {
	return n.contactDomain().GetContacts()
}

func (n *NodeApi) GetMessageRequests() ([]contacts.Request, error) {
	return n.contactDomain().GetMessageRequests()
}

func (n *NodeApi) NewContact(contact dto.ContactDTO) error {
	return n.contactDomain().NewContact(contact)
}

func (n *NodeApi) SetContactProfile(peerID, alias, avatarBase64 string) error {
	return n.contactDomain().SetContactProfile(peerID, alias, avatarBase64)
}

func (n *NodeApi) UnblockPeer(peerID string) error {
	return n.contactDomain().UnblockPeer(peerID)
}
