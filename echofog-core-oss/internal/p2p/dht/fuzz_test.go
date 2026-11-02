package dht

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/p2p/vpnmarket"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// FuzzCheckRecord: a stored or returned DHT record is bytes from any node. Checking it must not
// panic (the validator logs slices of the signature and key), and garbage must not pass.
func FuzzCheckRecord(f *testing.F) {
	priv := ed25519.NewKeyFromSeed(make([]byte, 32))
	good := fuzzRecord(priv, 0, uint64(time.Now().Unix()), 3600, []byte("v"))
	raw, _ := proto.Marshal(good)
	f.Add(raw)
	f.Add([]byte{})
	short, _ := proto.Marshal(&internal_pb.DhtRecord{Signature: make([]byte, 64), PublisherPubKey: make([]byte, 32)})
	f.Add(short)
	f.Fuzz(func(t *testing.T, raw []byte) {
		var rec internal_pb.DhtRecord
		if proto.Unmarshal(raw, &rec) != nil {
			return
		}
		if CheckRecord(&rec, nil) == nil && recordExpired(&rec, time.Now()) {
			t.Fatalf("an expired record passed the check: ts %d ttl %d", rec.Timestamp, rec.Ttl)
		}
	})
}

// FuzzSignedRecordTimes signs records with any timestamp and TTL, as their real owner may: an
// accepted one must be live, not from the future and within MaxTTL, whatever the integer sizes.
func FuzzSignedRecordTimes(f *testing.F) {
	now := uint64(time.Now().Unix())
	f.Add(uint8(0), now, uint32(3600))
	f.Add(uint8(1), now+59, uint32(1))
	f.Add(uint8(0), uint64(1)<<63, uint32(1<<31))
	f.Add(uint8(1), ^uint64(0), ^uint32(0))
	f.Fuzz(func(t *testing.T, which uint8, ts uint64, ttl uint32) {
		priv := ed25519.NewKeyFromSeed(make([]byte, 32))
		rec := fuzzRecord(priv, which, ts, ttl, []byte("value"))
		if err := CheckRecord(rec, nil); err != nil {
			return
		}
		n := time.Now().Unix()
		switch {
		case recordExpired(rec, time.Now()):
			t.Fatalf("expired record accepted: ts %d ttl %d", ts, ttl)
		case int64(ts) > n+61 || ts > uint64(n)+61:
			t.Fatalf("record from the future accepted: ts %d now %d", ts, n)
		case time.Duration(ttl)*time.Second > MaxTTL:
			t.Fatalf("record with a TTL over the maximum accepted: %d", ttl)
		}
	})
}

// fuzzRecord is a record of priv's node under one of its own keys (0: bundle, 1: exit advert).
func fuzzRecord(priv ed25519.PrivateKey, which uint8, ts uint64, ttl uint32, value []byte) *internal_pb.DhtRecord {
	pub := priv.Public().(ed25519.PublicKey)
	owner := types.PeerPubKeyToID(types.PeerPublicKey(pub))
	key := swarm.BundleKey(owner)
	if which%2 == 1 {
		key = vpnmarket.Key(owner)
	}
	rec := &internal_pb.DhtRecord{Key: key, Value: value, PublisherPubKey: pub, Timestamp: ts, Ttl: ttl}
	rec.Signature = ed25519.Sign(priv, PackRecordForSigning(rec.Key, rec.Value, rec.Timestamp, rec.Ttl))
	return rec
}
