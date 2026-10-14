package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/echofog/echofog-core/pkg/dto"
)

// AddContactByKey saves a contact from the public key the other person shared (64 hex characters).
// The peer ID is derived from the key, so a mistyped key cannot point at someone else.
func (a *App) AddContactByKey(pubKeyHex string, alias string) (dto.ContactDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return dto.ContactDTO{}, errors.New("node not started")
	}
	pubKeyHex = strings.ToLower(strings.TrimSpace(pubKeyHex))
	key, err := hex.DecodeString(pubKeyHex)
	if err != nil || len(key) != 32 {
		return dto.ContactDTO{}, errors.New("a contact key is 64 hexadecimal characters")
	}
	if pubKeyHex == nodeAPI.GetMyPubKey() {
		return dto.ContactDTO{}, errors.New("this is your own key")
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return dto.ContactDTO{}, errors.New("enter a name for the contact")
	}
	id := sha256.Sum256(key)
	c := dto.ContactDTO{PeerID: hex.EncodeToString(id[:]), PubKey: pubKeyHex, Alias: alias}
	if err := nodeAPI.NewContact(c); err != nil {
		return dto.ContactDTO{}, err
	}
	return c, nil
}
