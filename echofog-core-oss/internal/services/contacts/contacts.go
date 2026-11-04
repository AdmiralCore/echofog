package contacts

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/storage"
	apitypes "github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/dto"
)

type ContactService struct {
	storage storage.Storage
	// wire is the network side (see Wire); nil on a node without one, or in tests.
	wire Wire
}

func NewContactService(storage storage.Storage, wire Wire) *ContactService {
	return &ContactService{
		storage: storage,
		wire:    wire,
	}
}

func (c *ContactService) Name() string {
	return "ContactService"
}

func (c *ContactService) Start(ctx context.Context) error {
	return nil
}

func (c *ContactService) Stop() error {
	return nil
}

func (c *ContactService) SetPeerAlias(peerID string, alias string) error {
	return c.storage.Set(c.aliasKey(peerID), []byte(alias))
}

func (c *ContactService) GetPeerAlias(peerID string) (string, error) {
	val, err := c.storage.Get(c.aliasKey(peerID))
	if err != nil {
		return "", err
	}
	return string(val), nil
}

func (c *ContactService) UpdateContact(contact *dto.ContactDTO) error {
	if contact == nil || !c.Exists(contact.PeerID) {
		return errors.New("not a contact")
	}
	return c.NewContact(*contact)
}

// SetProfile renames a contact and sets the picture shown for it ("" removes it).
func (c *ContactService) SetProfile(peerID, alias, avatarBase64 string) error {
	list, err := c.GiveMyContacts()
	if err != nil {
		return err
	}
	for _, ct := range list {
		if ct.PeerID == peerID {
			if alias != "" {
				ct.Alias = alias
			}
			ct.Avatar = avatarBase64
			return c.UpdateContact(&ct)
		}
	}
	return errors.New("not a contact")
}

func (c *ContactService) RemoveContact(peerID string) error {
	return c.storage.Delete(c.contactKey(peerID))
}

func (c *ContactService) GiveMyContacts() ([]dto.ContactDTO, error) {
	findValues, err := c.storage.FindValues([]byte("contacts:"), 0)
	if err != nil {
		return nil, err
	}

	contacts := make([]dto.ContactDTO, 0, len(findValues))

	for _, v := range findValues {
		contact, err := dto.ContactFromBytes(v.Value)
		if err == nil {
			contacts = append(contacts, contact)
		}
	}

	return contacts, nil
}

func (c *ContactService) NewContact(contactDTO dto.ContactDTO) error {

	contactData, err := contactDTO.ToBytes()
	if err != nil {
		return err
	}

	return c.storage.Set(c.contactKey(contactDTO.PeerID), contactData)
}

func (c *ContactService) Exists(contactID string) bool {
	return c.storage.Exists(c.contactKey(contactID))
}

// Online reports whether the network has a live session with peerID (its 64-character hex form).
// A node without a wire — a server-role node, or a test — knows no reachable peer and answers
// false; a malformed ID does too. This is the one question contacts asks the network (Wire).
func (c *ContactService) Online(peerID string) bool {
	if c.wire == nil {
		return false
	}
	id, err := apitypes.PeerIDFromString(peerID)
	if err != nil {
		return false
	}
	return c.wire.Connected(id)
}

// The peer ID in a key is blinded, so the database does not list whom we know.
func (c *ContactService) contactKey(peerID string) []byte {
	return []byte("contacts:" + storage.BlindID(c.storage, peerID))
}

func (c *ContactService) aliasKey(peerID string) []byte {
	return []byte("peer:alias:" + storage.BlindID(c.storage, peerID))
}
