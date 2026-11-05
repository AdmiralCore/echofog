package noise3

import (
	"bytes"
	"testing"
)

// TestSetMaskKeysReplacesLiveKeys covers the runtime's in-place key update (T3.6b): a revoked
// invitation key must disappear from the accepted set without restarting the UDP listener.
func TestSetMaskKeysReplacesLiveKeys(t *testing.T) {
	l := &Listener{}
	first := MaskKey{Private: [32]byte{1, 2, 3}}
	second := MaskKey{Private: [32]byte{4, 5, 6}}

	if err := l.SetMaskKeys([]MaskKey{first}); err != nil {
		t.Fatalf("SetMaskKeys(first): %v", err)
	}
	pub1, err := maskPublic(first.Private)
	if err != nil {
		t.Fatalf("maskPublic: %v", err)
	}
	keys := l.maskKeys()
	if len(keys) != 1 || !bytes.Equal(keys[0].public[:], pub1[:]) {
		t.Fatal("the first key is not live")
	}

	if err := l.SetMaskKeys([]MaskKey{second}); err != nil {
		t.Fatalf("SetMaskKeys(second): %v", err)
	}
	pub2, err := maskPublic(second.Private)
	if err != nil {
		t.Fatalf("maskPublic: %v", err)
	}
	keys = l.maskKeys()
	if len(keys) != 1 || !bytes.Equal(keys[0].public[:], pub2[:]) {
		t.Fatal("the second key did not replace the first")
	}

	if err := l.SetMaskKeys(nil); err == nil {
		t.Fatal("an empty key set was accepted")
	}
	tooMany := make([]MaskKey, MaxMaskKeys+1)
	if err := l.SetMaskKeys(tooMany); err == nil {
		t.Fatal("more than the key limit was accepted")
	}
}
