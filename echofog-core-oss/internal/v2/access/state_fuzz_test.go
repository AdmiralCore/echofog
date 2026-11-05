package access

import "testing"

// FuzzDecodeInvitation checks the invitation record parser: length-prefixed fields, a bounded card
// and no panic on arbitrary state bytes (store values are attacker-controlled only locally, but a
// damaged disk must not crash the node).
func FuzzDecodeInvitation(f *testing.F) {
	rec := &invitationRecord{
		PassID:        PassID{1, 2, 3, 4, 5, 6, 7, 8},
		Secret:        make([]byte, 16),
		KeyID:         [8]byte{9, 8, 7, 6, 5, 4, 3, 2},
		Slot:          1,
		MaxEntries:    2,
		Card:          []byte{1, 2, 3},
		LinkExpires:   1_800_000_000,
		AccessExpires: 1_800_100_000,
	}
	f.Add(encodeInvitation(rec))
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{1, 1, 2, 3, 4})
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := decodeInvitation(b)
		if err != nil {
			return
		}
		if len(got.Secret) != 0 && len(got.Secret) != 16 {
			t.Fatalf("secret of %d bytes", len(got.Secret))
		}
		if len(got.Card) > maxInvitationCard {
			t.Fatalf("card of %d bytes", len(got.Card))
		}
		_ = encodeInvitation(got)
	})
}

// FuzzDecodePersonal checks the personal pass parser.
func FuzzDecodePersonal(f *testing.F) {
	rec := &personalRecord{
		PassID:      PassID{1, 2, 3, 4, 5, 6, 7, 8},
		Secret:      make([]byte, 16),
		Expires:     1_800_000_000,
		LastSeen:    1_700_000_000,
		CardVersion: 3,
	}
	f.Add(encodePersonal(rec))
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{1, 1, 2, 3, 4})
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := decodePersonal(b)
		if err != nil {
			return
		}
		if len(got.Secret) != 0 && len(got.Secret) != 16 {
			t.Fatalf("secret of %d bytes", len(got.Secret))
		}
		_ = encodePersonal(got)
	})
}

// FuzzDecodeMeta checks the small contacts-key pointer record.
func FuzzDecodeMeta(f *testing.F) {
	m := &metaRecord{CurrentKey: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, ContactsVersion: 2}
	f.Add(encodeMeta(m))
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{1, 1, 2, 3, 4})
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := decodeMeta(b)
		if err != nil {
			return
		}
		_ = encodeMeta(got)
	})
}

func FuzzDecodeKey(f *testing.F) {
	rec := &keyRecord{
		ID:       [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		Kind:     keyInvitation,
		Private:  make([]byte, 32),
		Slot:     2,
		NotAfter: 1_800_000_000,
		Version:  4,
	}
	f.Add(encodeKey(rec))
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{1, 1, 2, 3, 4})
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := decodeKey(b)
		if err != nil {
			return
		}
		if len(got.Private) != 32 {
			t.Fatalf("private key of %d bytes", len(got.Private))
		}
		_ = encodeKey(got)
	})
}
