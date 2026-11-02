package swarm

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func bundleAccount(t *testing.T) (*keymanager.AccountManager, types.PeerPublicKey) {
	t.Helper()
	st := testutil.NewTestingBadgerStorage(t)
	t.Cleanup(st.CloseAndClear)
	root := make([]byte, 64)
	_, _ = rand.Read(root)
	acc, err := keymanager.NewKeyManager(root, st.St).GetAccount(0)
	if err != nil {
		t.Fatal(err)
	}
	_, pub, err := acc.GetIdentityKey()
	if err != nil {
		t.Fatal(err)
	}
	return acc, types.PeerPublicKey(pub)
}

// onlyFields keeps the fields of a protobuf message with the given numbers: what a node whose
// generated code knows only those fields makes of it.
func onlyFields(t *testing.T, raw []byte, keep func(protowire.Number) bool) []byte {
	t.Helper()
	var out []byte
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			t.Fatal("bad tag")
		}
		m := protowire.ConsumeFieldValue(num, typ, raw[n:])
		if m < 0 {
			t.Fatal("bad field")
		}
		if keep(num) {
			out = append(out, raw[:n+m]...)
		}
		raw = raw[n+m:]
	}
	return out
}

func classicFields(n protowire.Number) bool { return n <= 5 }

// A version 2 bundle on the wire: v0.2.0 reads fields 1-5 of it and its classic signature holds,
// a new node reads and checks the KEM prekey too, and a bundle as v0.2.0 writes it is still taken.
func TestBundleWireCompatibility(t *testing.T) {
	defer memguard.Purge()
	acc, pub := bundleAccount(t)
	b, err := acc.CreateJITBundle()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	s := &Swarm{}

	var pb internal_pb.PreKeyBundle
	if err := proto.Unmarshal(raw, &pb); err != nil {
		t.Fatal(err)
	}
	got, err := s.parseAndVerifyProtoBundle(pub, &pb)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasKEM() || got.Version != keymanager.BundleVersionPQ || !bytes.Equal(got.KEMPreKey, b.KEMPreKey) || len(got.OneTimeKeys) != 1 {
		t.Fatal("the KEM prekey did not survive the wire")
	}

	// what v0.2.0 sees: the classic fields, and they verify as they always did
	var old internal_pb.PreKeyBundle
	if err := proto.Unmarshal(onlyFields(t, raw, classicFields), &old); err != nil {
		t.Fatal(err)
	}
	if len(old.GetKemPreKey()) != 0 {
		t.Fatal("the classic view must not hold the KEM prekey")
	}
	classic, err := s.parseAndVerifyProtoBundle(pub, &old)
	if err != nil {
		t.Fatalf("v0.2.0 would refuse the new bundle: %v", err)
	}
	if classic.HasKEM() || classic.SignedPreKey != b.SignedPreKey {
		t.Fatal("the classic view is the same bundle without the KEM prekey")
	}

	// a bundle of v0.2.0 itself: fields 1-5 only, also from EncodeBundle for a classic bundle
	b.Version, b.KEMPreKey, b.KEMSignature = 0, nil, [64]byte{}
	v020, err := EncodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(v020, onlyFields(t, raw, classicFields)) {
		t.Fatal("a classic bundle must be encoded exactly as v0.2.0 encodes it")
	}

	// one byte of the KEM prekey changed on the way: the whole bundle is refused, not used classic
	pb.KemPreKey = append([]byte(nil), pb.KemPreKey...)
	pb.KemPreKey[7] ^= 1
	if _, err := s.parseAndVerifyProtoBundle(pub, &pb); err == nil {
		t.Fatal("a bundle with a tampered KEM prekey was accepted")
	}
}

// A peer that is away: a cached bundle without a KEM prekey (signed before the peer moved to
// PQXDH) is not used while the network has a newer one, and is kept when the network has nothing
// newer. A cached PQ bundle of the day is used without asking.
func TestStaleCachedBundleAsksTheNetworkFirst(t *testing.T) {
	defer memguard.Purge()
	acc, pub := bundleAccount(t)
	id := types.PeerPubKeyToID(pub)
	st := testutil.NewTestingBadgerStorage(t)
	t.Cleanup(st.CloseAndClear)
	s := &Swarm{storage: st.St}
	record := func(b *keymanager.PublicBundle) []byte {
		raw, err := EncodeBundle(b)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := json.Marshal(StoredBundleRecord{PeerID: id, PubKey: pub, BundleRaw: raw})
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	pq, err := acc.CreateMyBundle(nil)
	if err != nil {
		t.Fatal(err)
	}
	classic := *pq
	classic.Version, classic.KEMPreKey, classic.KEMSignature = 0, nil, [64]byte{}
	if err := s.CacheBundle(id, record(&classic)); err != nil {
		t.Fatal(err)
	}

	fetched := 0
	var dht []byte
	s.SetBundleFetcher(func([]byte) ([]byte, error) {
		fetched++
		if dht == nil {
			return nil, errors.New("not found")
		}
		return dht, nil
	})
	// the network has nothing: the cached classic bundle is still better than none
	if b, err := s.GetBundle(id); err != nil || b.HasKEM() {
		t.Fatalf("the cached bundle when the network has none: %v", err)
	}
	dht = record(pq)
	b, err := s.GetBundle(id)
	if err != nil || !b.HasKEM() {
		t.Fatalf("a cached classic bundle won over the peer's PQ bundle in the network: %v", err)
	}
	// now the PQ bundle is cached, and taken without asking
	before := fetched
	if b, err := s.GetBundle(id); err != nil || !b.HasKEM() || fetched != before {
		t.Fatalf("a fresh cached PQ bundle: %v, %d lookups", err, fetched-before)
	}
}
