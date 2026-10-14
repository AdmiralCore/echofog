package swarm

import (
	"encoding/json"
	"errors"

	"time"

	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"

	"google.golang.org/protobuf/proto"
)

type StoredBundleRecord struct {
	PeerID    types.PeerID        `json:"peer_id"`
	PubKey    types.PeerPublicKey `json:"pub_key"`
	BundleRaw []byte              `json:"bundle_raw"`
}

// BundleFetcher looks a value up in the network (the DHT): the key bundles of peers that are not
// connected, which is the case for the first message to somebody behind another NAT.
type BundleFetcher func(key []byte) ([]byte, error)

// SetBundleFetcher sets the function used to fetch a peer's prekey bundle from the DHT when it is not cached.
func (s *Swarm) SetBundleFetcher(f BundleFetcher) {
	s.mu.Lock()
	s.fetchBundle = f
	s.mu.Unlock()
}

// The proto bundle has one field for one-time prekeys; several are stored back to back in it.
func joinOPKs(keys [][32]byte) []byte {
	out := make([]byte, 0, 32*len(keys))
	for _, k := range keys {
		out = append(out, k[:]...)
	}
	return out
}

func splitOPKs(raw []byte) ([][32]byte, error) {
	if len(raw)%32 != 0 || len(raw)/32 > keymanager.MaxBundleOPKs {
		return nil, errors.New("bad one-time prekey list")
	}
	out := make([][32]byte, len(raw)/32)
	for i := range out {
		copy(out[i][:], raw[i*32:])
	}
	return out, nil
}

// ForgetBundle drops the cached bundle of a peer, so the next session starts from a fresh one.
func (s *Swarm) ForgetBundle(id types.PeerID) {
	_ = s.storage.Delete(s.bundleCacheKey(id))
}

// bundleCacheKey is where we keep the bundle of a peer we wrote to; the peer is blinded in it.
func (s *Swarm) bundleCacheKey(id types.PeerID) []byte {
	return []byte("bcache:" + storage.BlindID(s.storage, id.String()))
}

// CacheBundle keeps a bundle record (as CreateMyPreKeyBundle makes it) of a peer, for sessions
// with it while it cannot be reached.
func (s *Swarm) CacheBundle(id types.PeerID, record []byte) error {
	if _, err := s.bundleFromRecord(id, record); err != nil {
		return err
	}
	return s.storage.Set(s.bundleCacheKey(id), record)
}

// BundleKey is where a peer's key bundle lives in the DHT.
func BundleKey(id types.PeerID) []byte { return []byte("bundle:" + id.String()) }

// bundleFromRecord checks a stored bundle record: the key must belong to the peer and the bundle
// must be signed by it.
func (s *Swarm) bundleFromRecord(peerID types.PeerID, data []byte) (*keymanager.PublicBundle, error) {
	var record StoredBundleRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	if types.PeerPubKeyToID(record.PubKey) != peerID {
		return nil, errors.New("the bundle does not belong to this peer")
	}
	var protoBundle internal_pb.PreKeyBundle
	if err := proto.Unmarshal(record.BundleRaw, &protoBundle); err != nil {
		return nil, err
	}
	return s.parseAndVerifyProtoBundle(record.PubKey, &protoBundle)
}

// GetBundle returns a peer's X3DH prekey bundle, from the cache or by fetching and verifying it.
func (s *Swarm) GetBundle(peerID types.PeerID) (*keymanager.PublicBundle, error) {
	s.mu.RLock()
	peer, ok := s.activePeers[peerID]
	s.mu.RUnlock()

	if !ok {
		// Fallback: check local storage cache if peer is not currently connected
		if data, err := s.storage.Get(s.bundleCacheKey(peerID)); err == nil && len(data) > 0 {
			if bundle, err := s.bundleFromRecord(peerID, data); err == nil {
				return bundle, nil
			}
			// expired or damaged: ask the network for a fresh one
		}
		// not connected and not cached: ask the network, the peer published its bundle there
		s.mu.RLock()
		fetch := s.fetchBundle
		s.mu.RUnlock()
		if fetch != nil {
			if data, err := fetch(BundleKey(peerID)); err == nil && len(data) > 0 {
				if bundle, err := s.bundleFromRecord(peerID, data); err == nil {
					_ = s.storage.Set(s.bundleCacheKey(peerID), data)
					return bundle, nil
				}
			}
		}
		return nil, errors.New("peer not connected and bundle not found in storage or in the network")
	}

	resType, resData, err := peer.transport.SendDataWithRequest(
		net_types.TypeLocalPublicKeyBundleRequest,
		nil,
	)
	if err != nil {
		return nil, err
	}

	if resType != net_types.TypeLocalPublicKeyBundleResponse {
		return nil, errors.New("unexpected response type for bundle request")
	}
	var protoBundle internal_pb.PreKeyBundle
	if err = proto.Unmarshal(resData, &protoBundle); err != nil {
		return nil, err
	}

	// Cache bundle in storage for offline/relay use
	record := StoredBundleRecord{
		PeerID:    peerID,
		PubKey:    peer.PubKey(),
		BundleRaw: resData,
	}
	if recBytes, err := json.Marshal(record); err == nil {
		_ = s.storage.Set(s.bundleCacheKey(peerID), recBytes)
	}

	return s.parseAndVerifyProtoBundle(peer.PubKey(), &protoBundle)
}

func (s *Swarm) parseAndVerifyProtoBundle(pubKey types.PeerPublicKey, protoBundle *internal_pb.PreKeyBundle) (*keymanager.PublicBundle, error) {
	var idKey, spkKey [32]byte
	var sig [64]byte

	copy(idKey[:], protoBundle.IdentityKey)
	copy(spkKey[:], protoBundle.SignedPreKey)
	copy(sig[:], protoBundle.Signature)
	opks, err := splitOPKs(protoBundle.OneTimePreKey)
	if err != nil {
		return nil, err
	}

	var peerKey [32]byte
	copy(peerKey[:], pubKey[:])

	bundle := &keymanager.PublicBundle{
		PeerID:       peerKey,
		IdentityKey:  idKey,
		SignedPreKey: spkKey,
		Signature:    sig,
		OneTimeKeys:  opks,
		CreatedAt:    protoBundle.CreatedAt,
	}

	if err := keymanager.VerifyBundle(bundle); err != nil {
		return nil, errors.New("received invalid or forged bundle: " + err.Error())
	}

	return bundle, nil
}

// IdentityCard is our signed bundle record without one-time prekeys. An init packet carries it, so
// the receiver can check our identity key without looking us up. It is made again once a day.
func (s *Swarm) IdentityCard() ([]byte, error) {
	s.cardMu.Lock()
	defer s.cardMu.Unlock()
	if s.card != nil && time.Since(s.cardAge) < 24*time.Hour {
		return s.card, nil
	}
	account, err := s.identity.GetActiveAccount()
	if err != nil {
		return nil, err
	}
	bundle, err := account.CreateMyBundle(nil)
	if err != nil {
		return nil, err
	}
	raw, err := proto.Marshal(&internal_pb.PreKeyBundle{
		IdentityKey:  bundle.IdentityKey[:],
		SignedPreKey: bundle.SignedPreKey[:],
		Signature:    bundle.Signature[:],
		CreatedAt:    bundle.CreatedAt,
	})
	if err != nil {
		return nil, err
	}
	card, err := json.Marshal(StoredBundleRecord{PeerID: s.selfID, PubKey: s.GetMyPubKey(), BundleRaw: raw})
	if err != nil {
		return nil, err
	}
	s.card, s.cardAge = card, time.Now()
	return card, nil
}

// BundleFromRecord checks a bundle record that claims to be peerID's: the key must hash to peerID,
// the bundle must be signed by it and not expired.
func (s *Swarm) BundleFromRecord(peerID types.PeerID, record []byte) (*keymanager.PublicBundle, error) {
	return s.bundleFromRecord(peerID, record)
}

// CreateMyPreKeyBundle builds and signs this node's own prekey bundle for publishing.
func (s *Swarm) CreateMyPreKeyBundle() ([]byte, error) {
	account, err := s.identity.GetActiveAccount()
	if err != nil {
		return nil, err
	}

	// published for everyone, so without one-time prekeys: peers starting sessions at the same
	// time would pick the same one, and only the first would get through. A one-time prekey is
	// handed out only on a direct request, to that peer alone.
	bundle, err := account.CreateMyBundle(nil)
	if err != nil {
		return nil, err
	}

	protoBundle := &internal_pb.PreKeyBundle{
		IdentityKey:   bundle.IdentityKey[:],
		SignedPreKey:  bundle.SignedPreKey[:],
		Signature:     bundle.Signature[:],
		OneTimePreKey: joinOPKs(bundle.OneTimeKeys),
		CreatedAt:     bundle.CreatedAt,
	}

	rawProto, err := proto.Marshal(protoBundle)
	if err != nil {
		return nil, err
	}

	record := StoredBundleRecord{
		PeerID:    s.selfID,
		PubKey:    s.selfPubKey,
		BundleRaw: rawProto,
	}

	return json.Marshal(record)
}
