package messenger

import (
	"bytes"
	"testing"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

func wireRoundTrip(t *testing.T, p *internal_pb.X3DHInit) types.X3DHInit {
	t.Helper()
	raw, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back internal_pb.X3DHInit
	if err := proto.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	return initFromProto(&back)
}

func TestInitPacketWire(t *testing.T) {
	first := types.EncryptedMessage{Header: types.RatchetHeader{DH: types.RatchetPublicKey{9}, N: 0}, Ciphertext: []byte("sealed")}
	opk, kemID := [32]byte{3}, [32]byte{4}
	pq := types.X3DHInit{
		EphemeralKey: [32]byte{1}, IdentityKey: [32]byte{2}, OneTimeKeyID: &opk,
		KEMCiphertext: bytes.Repeat([]byte{5}, 1088), KEMKeyID: &kemID, BundleCreatedAt: 1234,
	}
	got := wireRoundTrip(t, initToProto(&pq, first, []byte("card")))
	if got.EphemeralKey != pq.EphemeralKey || got.IdentityKey != pq.IdentityKey || *got.OneTimeKeyID != opk ||
		!bytes.Equal(got.KEMCiphertext, pq.KEMCiphertext) || got.KEMKeyID == nil || *got.KEMKeyID != kemID ||
		got.BundleCreatedAt != 1234 || !bytes.Equal(got.SecureMessage.Ciphertext, first.Ciphertext) || got.SecureMessage.Header.DH != first.Header.DH {
		t.Fatalf("a PQXDH init did not survive the wire: %+v", got)
	}

	// an init as v0.2.0 sends it: no KEM fields, no bundle time
	old := &internal_pb.X3DHInit{
		EphemeralKey: pq.EphemeralKey[:], IdentityKey: pq.IdentityKey[:], SenderBundle: []byte("card"),
		InitialCiphertext: &internal_pb.SecureMessage{Header: &internal_pb.RatchetHeader{RatchetKey: first.Header.DH[:]}, Ciphertext: first.Ciphertext},
	}
	got = wireRoundTrip(t, old)
	if got.KEMCiphertext != nil || got.KEMKeyID != nil || got.BundleCreatedAt != 0 || got.OneTimeKeyID != nil {
		t.Fatalf("an init of v0.2.0 must read as classic: %+v", got)
	}

	// a classic init of a new node is the v0.2.0 one plus the bundle time, which v0.2.0 skips
	classic := types.X3DHInit{EphemeralKey: pq.EphemeralKey, IdentityKey: pq.IdentityKey}
	newRaw, _ := proto.Marshal(initToProto(&classic, first, []byte("card")))
	oldRaw, _ := proto.Marshal(old)
	if !bytes.Equal(newRaw, oldRaw) {
		t.Fatal("a classic init without bundle time must be byte for byte the v0.2.0 one")
	}

	// a KEM key id of the wrong length names no key: it is kept, zero, for the X3DH layer to refuse
	bad := initToProto(&pq, first, nil)
	bad.KemKeyId = []byte{1, 2, 3}
	got = wireRoundTrip(t, bad)
	if got.KEMKeyID == nil || *got.KEMKeyID != ([32]byte{}) {
		t.Fatal("a malformed KEM key id must not vanish (that would read the init as classic)")
	}

	// no first message at all: read as empty, refused later, never a crash
	if got := initFromProto(&internal_pb.X3DHInit{}); len(got.SecureMessage.Ciphertext) != 0 {
		t.Fatal("an empty init has no message")
	}
}
