package contacts

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/dto"
)

type ContactService struct {
	storage storage.Storage
	sw      *swarm.Swarm
}

func NewContactService(storage storage.Storage, sw *swarm.Swarm) *ContactService {
	return &ContactService{
		storage: storage,
		sw:      sw,
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
	return errors.New("")
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

// The peer ID in a key is blinded, so the database does not list whom we know.
func (c *ContactService) contactKey(peerID string) []byte {
	return []byte("contacts:" + storage.BlindID(c.storage, peerID))
}

func (c *ContactService) aliasKey(peerID string) []byte {
	return []byte("peer:alias:" + storage.BlindID(c.storage, peerID))
}
