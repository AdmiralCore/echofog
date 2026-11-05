package dht

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// FuzzClientAnswers feeds arbitrary bytes to everything the lookup client parses from the network:
// the response frames and the records and cards inside them. Nothing may panic, and an accepted
// record must carry the asked label and signer.
func FuzzClientAnswers(f *testing.F) {
	sg := seededSigner(f, 5)
	rec := hintRecord(f, sg, 1, srvNow.Add(time.Hour))
	sid := signerID(sg)
	f.Add(uint8(0), rec, sid[:])
	f.Add(uint8(1), []byte{0x08, 0x01, 0x12, 0x03, 'a', 'b', 'c'}, sid[:])
	f.Add(uint8(2), []byte{}, make([]byte, 32))
	f.Add(uint8(3), []byte{0x08, 0x01, 0x10, 0x04}, make([]byte, 32))
	f.Fuzz(func(t *testing.T, kind uint8, data []byte, signerRaw []byte) {
		var signer [32]byte
		copy(signer[:], signerRaw)
		switch kind % 4 {
		case 0:
			if env, err := acceptRecord(objects.LabelMailboxHint, signer, data, srvNow, nil); err == nil {
				if env.Label != objects.LabelMailboxHint || env.SignerID() != signer {
					t.Fatalf("accepted a record of %q by %x", env.Label, env.SignerID())
				}
			}
		case 1:
			_, _, _ = DecodeValue(1, TypeValue, data)
		case 2:
			_, _ = DecodeNodes(1, TypeNodes, data)
		case 3:
			_, _ = DecodeStored(1, TypeStored, data)
			_, _ = DecodeStored(1, TypeError, data)
		}
	})
}
