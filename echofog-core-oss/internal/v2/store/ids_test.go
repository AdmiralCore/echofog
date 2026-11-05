package store

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/secret"
)

// TestIDsAreDeterministic pins that two blinders of the same store key produce the same id for the
// same raw value and domain: the id is a stable key, not a random token.
func TestIDsAreDeterministic(t *testing.T) {
	t.Parallel()

	key := testKey(t, 21)
	first, err := NewIDs(key)
	if err != nil {
		t.Fatalf("NewIDs: %v", err)
	}
	t.Cleanup(first.Close)
	second, err := NewIDs(key)
	if err != nil {
		t.Fatalf("second NewIDs: %v", err)
	}
	t.Cleanup(second.Close)

	raw := []byte("node id of a contact")
	a, err := first.Blind(DomainPeer, raw)
	if err != nil {
		t.Fatalf("Blind: %v", err)
	}
	b, err := second.Blind(DomainPeer, raw)
	if err != nil {
		t.Fatalf("Blind from the second blinder: %v", err)
	}
	if a != b {
		t.Fatalf("Blind is not deterministic: %x != %x", a, b)
	}
}

// TestIDsAreDomainSeparated pins that a raw id used as a peer, a chat, a group and a device gives
// four unrelated values, so one namespace cannot be scanned with another's keys.
func TestIDsAreDomainSeparated(t *testing.T) {
	t.Parallel()

	ids, err := NewIDs(testKey(t, 22))
	if err != nil {
		t.Fatalf("NewIDs: %v", err)
	}
	t.Cleanup(ids.Close)

	raw := []byte("the same raw identifier")
	domains := []Domain{DomainPeer, DomainChat, DomainGroup, DomainDevice}
	seen := make(map[ID]Domain, len(domains))
	for _, domain := range domains {
		id, err := ids.Blind(domain, raw)
		if err != nil {
			t.Fatalf("Blind(%q): %v", domain, err)
		}
		if other, ok := seen[id]; ok {
			t.Fatalf("domains %q and %q returned the same id %x", other, domain, id)
		}
		seen[id] = domain
	}
}

// TestIDsDependOnTheStorageKey pins that another profile's key gives another id: identifiers are
// not reversible through a public function.
func TestIDsDependOnTheStorageKey(t *testing.T) {
	t.Parallel()

	raw := []byte("a contact")
	a, err := NewIDs(testKey(t, 23))
	if err != nil {
		t.Fatalf("NewIDs: %v", err)
	}
	t.Cleanup(a.Close)
	b, err := NewIDs(testKey(t, 24))
	if err != nil {
		t.Fatalf("NewIDs: %v", err)
	}
	t.Cleanup(b.Close)

	first, err := a.Blind(DomainPeer, raw)
	if err != nil {
		t.Fatalf("Blind with the first key: %v", err)
	}
	second, err := b.Blind(DomainPeer, raw)
	if err != nil {
		t.Fatalf("Blind with the second key: %v", err)
	}
	if first == second {
		t.Fatalf("two storage keys produced the same id %x", first)
	}
}

// TestBlindedIDIsNotTheRawValue pins that the value put in a key cannot be read back as the raw
// identifier, also in its text form.
func TestBlindedIDIsNotTheRawValue(t *testing.T) {
	t.Parallel()

	ids, err := NewIDs(testKey(t, 25))
	if err != nil {
		t.Fatalf("NewIDs: %v", err)
	}
	t.Cleanup(ids.Close)

	raw := []byte("alice@example.invalid")
	id, err := ids.Blind(DomainPeer, raw)
	if err != nil {
		t.Fatalf("Blind: %v", err)
	}
	if bytes.Equal(id[:], raw) {
		t.Fatal("the blinded id is the raw value")
	}
	if strings.Contains(id.String(), string(raw)) {
		t.Fatalf("the text form of the id contains the raw value: %s", id)
	}
	if id == (ID{}) {
		t.Fatal("the blinded id is all zeroes")
	}
}

// TestIDsDoNotLeakIntoErrors pins that the raw value is not echoed by errors: an error is likely to
// be logged, and ids and their sources must not end up in logs.
func TestIDsDoNotLeakIntoErrors(t *testing.T) {
	t.Parallel()

	ids, err := NewIDs(testKey(t, 26))
	if err != nil {
		t.Fatalf("NewIDs: %v", err)
	}
	t.Cleanup(ids.Close)

	raw := []byte("private peer identifier")
	if _, err := ids.Blind(Domain(""), raw); err == nil {
		t.Fatal("Blind with an empty domain succeeded")
	} else if strings.Contains(err.Error(), string(raw)) {
		t.Fatalf("the domain error repeats the raw id: %v", err)
	}

	ids.Close()
	_, err = ids.Blind(DomainPeer, raw)
	if !errors.Is(err, secret.ErrDestroyed) {
		t.Fatalf("Blind after Close = %v, want secret.ErrDestroyed", err)
	}
	if strings.Contains(err.Error(), string(raw)) {
		t.Fatalf("the closed error repeats the raw id: %v", err)
	}
}

func TestNewIDsRequiresAStorageKey(t *testing.T) {
	t.Parallel()

	short := secret.New([]byte{1, 2, 3})
	t.Cleanup(short.Destroy)

	for _, tc := range []struct {
		name string
		key  *secret.Value
	}{
		{"nil", nil},
		{"short", short},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewIDs(tc.key); !errors.Is(err, ErrBadKey) {
				t.Fatalf("NewIDs with a %s key: %v, want ErrBadKey", tc.name, err)
			}
		})
	}
}
