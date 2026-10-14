package dht

import (
	"bytes"
	"errors"

	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/p2p/vpnmarket"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

var ErrNotOwner = errors.New("the record key belongs to another node than the one that signed it")

// ownedKeys are the keys a node may publish under, derived from its own ID. A record is accepted
// only under one of the keys of its signer: nobody can publish a bundle or an exit advert in
// another node's name, and keys nobody owns are not stored at all.
func ownedKeys(owner types.PeerID) [][]byte {
	return [][]byte{
		swarm.BundleKey(owner),
		vpnmarket.Key(owner),
	}
}

// ValidateOwner checks that the record is published under a key of its signer.
func ValidateOwner(rec *internal_pb.DhtRecord) error {
	if len(rec.PublisherPubKey) != 32 {
		return ErrNotOwner
	}
	owner := types.PeerPubKeyToID(types.PeerPublicKey(rec.PublisherPubKey))
	for _, k := range ownedKeys(owner) {
		if bytes.Equal(k, rec.Key) {
			return nil
		}
	}
	return ErrNotOwner
}

// CheckRecord is everything a record from the network must pass: a valid signature, a key of the
// signer, and the key that was asked for (when one was).
func CheckRecord(rec *internal_pb.DhtRecord, wantKey []byte) error {
	if err := ValidateRecord(rec); err != nil {
		return err
	}
	if wantKey != nil && !bytes.Equal(rec.Key, wantKey) {
		return errors.New("record for another key than asked")
	}
	return ValidateOwner(rec)
}

// newerThan: a record replaces the one held only when it is not older, so an old record of the
// owner replayed by somebody cannot undo an update.
func newerThan(rec, held *internal_pb.DhtRecord) bool {
	return held == nil || rec.Timestamp >= held.Timestamp
}
