package dht

import (
	"crypto/ed25519"
	"crypto/mlkem"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// A version 2 bundle is about 1.2 KB bigger (the KEM prekey and its signature, base64 in the JSON
// record). It must still fit every limit on its way: the DHT record, and, as the card inside an
// init packet with the KEM ciphertext, the frames that carry chat messages. None of these goes
// through the handshake stream, whose frames are capped at network.HandshakeMaxFrameSize.
func TestPQBundleFitsTheLimits(t *testing.T) {
	defer memguard.Purge()
	st := testutil.NewTestingBadgerStorage(t)
	defer st.CloseAndClear()
	root := make([]byte, 64)
	_, _ = rand.Read(root)
	acc, err := keymanager.NewKeyManager(root, st.St).GetAccount(0)
	if err != nil {
		t.Fatal(err)
	}
	privEnc, pubSlice, err := acc.GetIdentityKey()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := privEnc.Open()
	if err != nil {
		t.Fatal(err)
	}
	priv := ed25519.PrivateKey(append([]byte(nil), locked.Bytes()...))
	locked.Destroy()
	pub := types.PeerPublicKey(pubSlice)
	id := types.PeerPubKeyToID(pub)

	// the largest record a node publishes: a bundle with a one-time prekey, four mailbox hints
	// with addresses of the longest kind accepted, and a site name
	b, err := acc.CreateJITBundle()
	if err != nil {
		t.Fatal(err)
	}
	record := func(b *keymanager.PublicBundle) []byte {
		raw, err := swarm.EncodeBundle(b)
		if err != nil {
			t.Fatal(err)
		}
		rec := swarm.StoredBundleRecord{PeerID: id, PubKey: pub, BundleRaw: raw, SNI: strings.Repeat("s", 253), MboxSig: make([]byte, 64)}
		for i := 0; i < 4; i++ {
			rec.Mbox = append(rec.Mbox, swarm.MailboxHint{Peer: strings.Repeat("ab", 32), Addr: strings.Repeat("a", 256), Until: time.Now().Unix()})
		}
		out, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	value := record(b)
	classic := *b
	classic.Version, classic.KEMPreKey, classic.KEMSignature = 0, nil, [64]byte{}
	grew := len(value) - len(record(&classic))
	t.Logf("bundle record: %d bytes (%d more than classic); DHT limit %d", len(value), grew, MaxRecordSize)
	if grew > 2000 {
		t.Fatalf("the KEM prekey added %d bytes, more than expected", grew)
	}

	rec := &internal_pb.DhtRecord{
		Key: swarm.BundleKey(id), Value: value, PublisherPubKey: pub[:],
		Timestamp: uint64(time.Now().Unix()), Ttl: 86400,
	}
	rec.Signature = ed25519.Sign(priv, PackRecordForSigning(rec.Key, rec.Value, rec.Timestamp, rec.Ttl))
	if err := CheckRecord(rec, swarm.BundleKey(id)); err != nil {
		t.Fatalf("the DHT refuses a PQ bundle record: %v", err)
	}
	if len(value) > MaxRecordSize/4 {
		t.Fatalf("a bundle record of %d bytes leaves too little room under %d", len(value), MaxRecordSize)
	}

	// an init packet: the KEM ciphertext, the sender's card (a record with the KEM prekey) and a
	// long first message, in a signed envelope as relays and mailboxes carry it
	card, err := acc.CreateMyBundle(nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := &internal_pb.MessageData{
		MessageId: make([]byte, 16), OriginId: id[:], TargetId: id[:], Timestamp: 1, HopLimit: 20,
		Payload: &internal_pb.MessageData_X3DhInit{X3DhInit: &internal_pb.X3DHInit{
			EphemeralKey: make([]byte, 32), IdentityKey: make([]byte, 32), OneTimeKeyId: make([]byte, 32),
			KemCiphertext: make([]byte, mlkem.CiphertextSize768), KemKeyId: make([]byte, 32), BundleCreatedAt: time.Now().Unix(),
			SenderBundle: record(card),
			InitialCiphertext: &internal_pb.SecureMessage{
				Header: &internal_pb.RatchetHeader{RatchetKey: make([]byte, 32), N: 0}, Ciphertext: make([]byte, 4096),
			},
		}},
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Marshal(&internal_pb.Envelope{Data: data, Signature: ed25519.Sign(priv, data), PubKey: pub[:]})
	if err != nil {
		t.Fatal(err)
	}
	// 64 KiB: the smallest of the limits on the way (an early relay frame; mailboxes take 1 MiB)
	t.Logf("init envelope with a 4 KiB first message: %d bytes", len(env))
	if len(env) > 16<<10 {
		t.Fatalf("an init packet of %d bytes", len(env))
	}
	// the card alone, as a knock carries it, is far below a frame of chat traffic too
	if len(record(card)) > network.MaxFrameSize/1024 {
		t.Fatal("a card bigger than expected")
	}
}
