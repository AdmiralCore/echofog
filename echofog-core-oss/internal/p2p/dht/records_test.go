package dht

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

func storeFromFreshIdentity(t *testing.T, d *EchoFogDHT, ts uint64, ttl uint32) []byte {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	owner := types.PeerPubKeyToID(types.PeerPublicKey(pub))
	key := swarm.BundleKey(owner)
	rec := &internal_pb.DhtRecord{Key: key, Value: []byte("v"), PublisherPubKey: pub, Timestamp: ts, Ttl: ttl}
	rec.Signature = ed25519.Sign(priv, PackRecordForSigning(rec.Key, rec.Value, rec.Timestamp, rec.Ttl))
	d.HandleStore(&internal_pb.MessageData{Payload: &internal_pb.MessageData_StoreReq{StoreReq: &internal_pb.StoreReq{Record: rec}}}, owner)
	return key
}

func TestSweepDeletesRecordsPastTheirTTL(t *testing.T) {
	n := newTestNode(t)
	now := time.Now()
	live := storeFromFreshIdentity(t, n.dht, uint64(now.Unix()), 3600)
	short := storeFromFreshIdentity(t, n.dht, uint64(now.Unix()), 2)
	st := n.sw.GetStorage()
	if !st.Exists(RecordKey(live)) || !st.Exists(RecordKey(short)) {
		t.Fatal("both records should be stored")
	}
	_ = st.Set([]byte(RecordPrefix+"junk"), []byte{0xff, 0xff})

	if got := n.dht.SweepRecords(now.Add(10 * time.Second)); got != 2 {
		t.Fatalf("swept %d, want the expired record and the unreadable one", got)
	}
	if !st.Exists(RecordKey(live)) || st.Exists(RecordKey(short)) {
		t.Fatal("the sweep must keep the live record and drop the expired one")
	}
	if n.dht.records != 1 {
		t.Fatalf("counted %d records, want 1", n.dht.records)
	}
}

func TestExpiredRecordIsNotServed(t *testing.T) {
	n := newTestNode(t)
	key := []byte("bundle:x")
	rec := &internal_pb.DhtRecord{Key: key, Timestamp: uint64(time.Now().Add(-2 * time.Hour).Unix()), Ttl: 60}
	raw, _ := proto.Marshal(rec)
	_ = n.sw.GetStorage().Set(RecordKey(key), raw)
	if _, ok := n.dht.localRecord(key); ok {
		t.Fatal("a record past its TTL must not be handed out")
	}
}

func TestFreshIdentitiesHitTheNodeWideCaps(t *testing.T) {
	n := newTestNode(t)
	d := n.dht
	d.SweepRecords(time.Now())

	d.recMu.Lock()
	d.records = MaxStoredRecords - 1
	d.recMu.Unlock()
	ts := uint64(time.Now().Unix())
	first := storeFromFreshIdentity(t, d, ts, 3600)
	second := storeFromFreshIdentity(t, d, ts, 3600)
	st := n.sw.GetStorage()
	if !st.Exists(RecordKey(first)) || st.Exists(RecordKey(second)) {
		t.Fatal("a full store takes no new record, from whatever identity")
	}

	d.recMu.Lock()
	d.records, d.storesHour = 0, MaxStoresPerHour
	d.recMu.Unlock()
	if third := storeFromFreshIdentity(t, d, ts, 3600); st.Exists(RecordKey(third)) {
		t.Fatal("past the hourly budget no new record is taken in")
	}
	d.ResetQuotas()
	if fourth := storeFromFreshIdentity(t, d, ts, 3600); !st.Exists(RecordKey(fourth)) {
		t.Fatal("the hourly budget comes back with the quota reset")
	}
}
