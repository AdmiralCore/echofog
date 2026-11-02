package signaling

import (
	"testing"
)

// FuzzDecodeSignal: call signals come from any participant (and through relays). Parsing and
// checking the signature must not panic, and an unsigned or forged signal must not verify.
func FuzzDecodeSignal(f *testing.F) {
	raw, _ := EncodeSignal("join", "call-1", [32]byte{1}, 3, map[string]int{"a": 1})
	f.Add(raw)
	f.Add([]byte(`{"action":"join","signature":"AAAA","sender_pub_key":[1]}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		env, err := DecodeSignal(data)
		if err != nil {
			return
		}
		if env.Verify() == nil {
			t.Fatalf("a signal nobody signed verified: %s", data)
		}
	})
}
