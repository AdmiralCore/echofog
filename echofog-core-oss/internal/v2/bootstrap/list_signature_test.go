package bootstrap_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
	"github.com/echofog/echofog-core/internal/v2/keys"
)

// listBodyAt is the first byte of the list body: magic, seq, not_after, key length, signer
// key and body length come before it.
func listBodyAt() int { return len("EFBL2\x00") + 8 + 8 + 1 + 33 + 4 }

// TestOpenListSignatureCoversTheBody: with a trusted signer, only keys.Verify can reject a list
// whose body, seq or term changed. The mutation that skips keys.Verify must make this test red.
func TestOpenListSignatureCoversTheBody(t *testing.T) {
	s := testSigner(t, 7)
	raw, err := bootstrap.SealList(s, 12, uint64(testNow.Add(30*24*time.Hour).Unix()), listA+"\n")
	if err != nil {
		t.Fatal(err)
	}
	trusted := []keys.AKey{s.Public()}

	// One host digit in the body: the list still parses, so only the signature can tell.
	body := raw[listBodyAt():]
	at := listBodyAt() + bytes.Index(body, []byte("192.0.2.10")) + len("192.0.2.")
	cases := []struct {
		name string
		off  int
	}{
		{"body", at},
		{"seq", len("EFBL2\x00")},
		{"term", len("EFBL2\x00") + 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := append([]byte(nil), raw...)
			bad[tc.off] ^= 0x01
			if _, err := bootstrap.OpenList(bad, trusted, testNow); !errors.Is(err, bootstrap.ErrListSignature) {
				t.Fatalf("a changed %s was not rejected by the signature: %v", tc.name, err)
			}
		})
	}
}

// TestOpenListForeignSignerIsRefused: a valid signature by a key outside the release set is not a
// trust root. The mutation that stops checking the signer must make this test red.
func TestOpenListForeignSignerIsRefused(t *testing.T) {
	raw, err := bootstrap.SealList(testSigner(t, 8), 12, uint64(testNow.Add(30*24*time.Hour).Unix()), listA+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.OpenList(raw, []keys.AKey{testSigner(t, 7).Public()}, testNow); !errors.Is(err, bootstrap.ErrListSignature) {
		t.Fatalf("a list signed by a foreign key was accepted: %v", err)
	}
}

// TestOpenListExpiredIsRefused: the term is checked. The mutation that skips it must make this red.
func TestOpenListExpiredIsRefused(t *testing.T) {
	s := testSigner(t, 7)
	raw, err := bootstrap.SealList(s, 12, uint64(testNow.Add(-time.Hour).Unix()), listA+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.OpenList(raw, []keys.AKey{s.Public()}, testNow); !errors.Is(err, bootstrap.ErrListExpired) {
		t.Fatalf("an expired list was accepted: %v", err)
	}
}

// TestManagerOfferStaleSeqIsRefused: an update must be newer than the effective list. The
// mutation that skips the seq check must make this test red.
func TestManagerOfferStaleSeqIsRefused(t *testing.T) {
	m, s := newManager(t, newMemKV()) // the built-in list has seq 10
	raw, err := bootstrap.SealList(s, 10, uint64(testNow.Add(time.Hour).Unix()), listA+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Offer(raw); !errors.Is(err, bootstrap.ErrListStale) {
		t.Fatalf("a list with the applied seq was accepted: %v", err)
	}
	if m.Seq() != 10 {
		t.Fatalf("the refused list changed the seq: %d", m.Seq())
	}
}
