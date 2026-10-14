package dht

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/p2p/vpnmarket"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func signedRecord(t *testing.T, priv ed25519.PrivateKey, key []byte, ts uint64) *internal_pb.DhtRecord {
	t.Helper()
	rec := &internal_pb.DhtRecord{Key: key, Value: []byte("v"), PublisherPubKey: priv.Public().(ed25519.PublicKey), Timestamp: ts, Ttl: 3600}
	rec.Signature = ed25519.Sign(priv, PackRecordForSigning(rec.Key, rec.Value, rec.Timestamp, rec.Ttl))
	return rec
}

func TestRecordsOnlyUnderTheSignersKeys(t *testing.T) {
	pubA, privA, _ := ed25519.GenerateKey(rand.Reader)
	pubB, _, _ := ed25519.GenerateKey(rand.Reader)
	a := types.PeerPubKeyToID(types.PeerPublicKey(pubA))
	b := types.PeerPubKeyToID(types.PeerPublicKey(pubB))
	now := uint64(time.Now().Unix())

	for _, ok := range [][]byte{swarm.BundleKey(a), vpnmarket.Key(a)} {
		if err := CheckRecord(signedRecord(t, privA, ok, now), nil); err != nil {
			t.Fatalf("own key %q refused: %v", ok, err)
		}
	}
	for _, bad := range [][]byte{swarm.BundleKey(b), vpnmarket.Key(b), []byte("contacts:x"), []byte("system:identity:root")} {
		if err := CheckRecord(signedRecord(t, privA, bad, now), nil); err == nil {
			t.Fatalf("record under %q accepted from a node that does not own it", bad)
		}
	}
	// the answer must be for the key that was asked
	if err := CheckRecord(signedRecord(t, privA, swarm.BundleKey(a), now), vpnmarket.Key(a)); err == nil {
		t.Fatal("a record for another key was accepted as the answer")
	}
}

func TestAnOlderRecordDoesNotReplaceANewerOne(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := uint64(time.Now().Unix())
	held := signedRecord(t, priv, []byte("k"), now)
	if newerThan(signedRecord(t, priv, []byte("k"), now-10), held) {
		t.Fatal("a replayed older record would replace the newer one")
	}
	if !newerThan(signedRecord(t, priv, []byte("k"), now+1), held) || !newerThan(held, nil) {
		t.Fatal("a newer record is refused")
	}
}
