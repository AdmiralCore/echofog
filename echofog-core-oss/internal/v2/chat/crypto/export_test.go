package chatcrypto

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/crypto/keymanager"
)

// ageKeysForTest makes the current signed prekey and KEM prekey look d older to the v1 key manager,
// which rotates by the wall clock (it has no injectable clock): the sealed records are rewritten
// with an earlier creation time, as if the process had run for d.
func (c *Crypto) ageKeysForTest(t *testing.T, d time.Duration) {
	t.Helper()
	key, err := c.acc.StorageKey()
	if err != nil {
		t.Fatal(err)
	}
	kv := c.keysKV()
	for _, name := range []string{"spk:current", "kem:current"} {
		sealed, err := kv.Get([]byte(name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		plain, err := keymanager.OpenValue(key, []byte(name), sealed)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		created := int64(binary.BigEndian.Uint64(plain[:8]))
		binary.BigEndian.PutUint64(plain[:8], uint64(time.Unix(created, 0).Add(-d).Unix()))
		again, err := keymanager.SealValue(key, []byte(name), plain)
		if err != nil {
			t.Fatal(err)
		}
		if err := kv.Put([]byte(name), again); err != nil {
			t.Fatal(err)
		}
	}
	// the KEM public half carries its own creation time
	if pub, err := kv.Get([]byte("kem:pub")); err == nil && len(pub) > 8 {
		created := int64(binary.BigEndian.Uint64(pub[:8]))
		binary.BigEndian.PutUint64(pub[:8], uint64(time.Unix(created, 0).Add(-d).Unix()))
		if err := kv.Put([]byte("kem:pub"), pub); err != nil {
			t.Fatal(err)
		}
	}
}
