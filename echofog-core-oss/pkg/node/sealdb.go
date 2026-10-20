package node

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"

	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/p2p/dht"
	"github.com/echofog/echofog-core/internal/storage"
)

// sealStorage puts the encrypting layer over the database once the account is unlocked, and
// converts what an older version stored in the open. Everything that starts after it reads and
// writes through the layer; only the identity root is read before, and it is encrypted itself.
func (n *Node) sealStorage(account *keymanager.AccountManager) error {
	if _, done := n.Storage.(*storage.Sealed); done {
		return nil
	}
	key, err := account.StorageKey()
	if err != nil {
		return err
	}
	sealed, err := storage.NewSealed(n.Storage, key)
	if err != nil {
		return err
	}
	moved := migrateToSealed(sealed)
	dropLegacyKeys(n.Storage)
	dropOpenDHTRecords(n.Storage)
	if moved > 0 {
		n.Logger.Info("Database: personal data encrypted", "records", moved)
	}
	n.Storage = sealed
	return nil
}

// migrateToSealed seals every value under the sealed prefixes that is still in the open and moves
// it under a key with blinded identifiers. A value that opens is done already, so an interrupted
// run simply continues. Returns how many records it converted.
func migrateToSealed(s *storage.Sealed) int {
	raw := s.Inner()
	moved := 0
	chats := map[uint64]bool{}
	for _, prefix := range storage.SealedPrefixes {
		entries, err := raw.FindValues(prefix, 0)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if s.Opens(e.Key, e.Value) {
				continue
			}
			newKey := blindedKey(s, e.Key, chats)
			if err := s.Set(newKey, e.Value); err != nil {
				continue
			}
			if !bytes.Equal(newKey, e.Key) {
				_ = raw.Delete(e.Key)
			}
			moved++
		}
	}
	// message numbers continue under the blinded chat
	for chat := range chats {
		oldKey := seqKey(chat)
		v, err := raw.Get(oldKey)
		if err != nil || len(v) != 8 {
			continue
		}
		newKey := seqKey(s.BlindChat(chat))
		if cur, err := raw.Get(newKey); err != nil || len(cur) != 8 || binary.BigEndian.Uint64(cur) < binary.BigEndian.Uint64(v) {
			_ = raw.Set(newKey, v)
		}
		_ = raw.Delete(oldKey)
	}
	return moved
}

// legacyKeys held peer IDs in the open: sessions and used prekeys (both sealed or rebuilt under
// blinded keys now), bundles of peers we wrote to, and the DHT records we looked up.
var legacyKeys = []string{"session:", "opk-used:", "bundle:", "dht:rec:bundle:"}

const legacyDroppedMarker = "system:sealed:legacy-dropped"

// dropLegacyKeys runs once: sessions start again, bundles are fetched again.
func dropLegacyKeys(raw storage.Storage) {
	if raw.Exists([]byte(legacyDroppedMarker)) {
		return
	}
	for _, p := range legacyKeys {
		entries, err := raw.FindValues([]byte(p), 0)
		if err != nil {
			continue
		}
		for _, e := range entries {
			_ = raw.Delete(e.Key)
		}
	}
	_ = raw.Set([]byte(legacyDroppedMarker), []byte{1})
}

// DHT records used to be stored under their key in the open, and the keys name peers
// ("bundle:<id>"). They now live under its hash (dht.RecordKey); the old ones are dropped once,
// the network hands them out again on the next lookup.
const openDHTDroppedMarker = "system:dht:open-keys-dropped"

func dropOpenDHTRecords(raw storage.Storage) {
	if raw.Exists([]byte(openDHTDroppedMarker)) {
		return
	}
	if entries, err := raw.FindValues([]byte(dht.RecordPrefix), 0); err == nil {
		for _, e := range entries {
			if !hashedRecordKey(e.Key) {
				_ = raw.Delete(e.Key)
			}
		}
	}
	_ = raw.Set([]byte(openDHTDroppedMarker), []byte{1})
}

func hashedRecordKey(key []byte) bool {
	rest := key[len(dht.RecordPrefix):]
	if len(rest) != 64 {
		return false
	}
	_, err := hex.DecodeString(string(rest))
	return err == nil
}

// seqKey is the message counter of a chat (storage.BadgerStorage.GetNextID).
func seqKey(chat uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte("seq:msg:"), chat)
}

// blindedKey is where an old record goes: the same key with the peer or chat in it blinded.
func blindedKey(s *storage.Sealed, key []byte, chats map[uint64]bool) []byte {
	k := string(key)
	switch {
	case strings.HasPrefix(k, "contacts:"):
		return []byte("contacts:" + s.BlindID(strings.TrimPrefix(k, "contacts:")))
	case strings.HasPrefix(k, "peer:alias:"):
		return []byte("peer:alias:" + s.BlindID(strings.TrimPrefix(k, "peer:alias:")))
	case strings.HasPrefix(k, "saved:peers:") && len(key) == len("saved:peers:")+32:
		return []byte("saved:peers:" + s.BlindID(hex.EncodeToString(key[len("saved:peers:"):])))
	case strings.HasPrefix(k, "outbox/"):
		rest := strings.TrimPrefix(k, "outbox/")
		if i := strings.IndexByte(rest, '/'); i > 0 {
			return []byte("outbox/" + s.BlindID(rest[:i]) + rest[i:])
		}
	case strings.HasPrefix(k, "m:") && len(key) == 30:
		chat := binary.BigEndian.Uint64(key[6:14])
		chats[chat] = true
		out := append([]byte(nil), key...)
		binary.BigEndian.PutUint64(out[6:14], s.BlindChat(chat))
		return out
	}
	return key
}
