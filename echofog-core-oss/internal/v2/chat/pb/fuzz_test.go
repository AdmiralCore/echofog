package chatpb

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
)

// FuzzParse covers every decoder: no panic, and what parses re-encodes to something that parses
// again.
func FuzzParse(f *testing.F) {
	for _, s := range samples(f) {
		f.Add(s)
	}
	f.Add(mustMarshal(f, goodInit()))
	f.Add(mustMarshal(f, goodMessage()))
	f.Add(mustMarshal(f, &BundleGet{Req: 3}))
	f.Add(mustMarshal(f, &Bundle{Req: 3, Prekeys: rep(1, 100)}))

	// Unknown fields are ignored (services.md 1), so the re-encoding may be shorter than the input;
	// it must parse again and be stable.
	same := func(t *testing.T, m proto.Message, parse func([]byte) (proto.Message, error)) {
		t.Helper()
		out := mustMarshal(t, m)
		again, err := parse(out)
		if err != nil {
			t.Fatalf("re-encoding of a parsed message does not parse: %v", err)
		}
		if !bytes.Equal(mustMarshal(t, again), out) {
			t.Fatal("re-encoding is not stable")
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if m, err := ParseSecureMessage(b); err == nil {
			same(t, m, func(x []byte) (proto.Message, error) { return ParseSecureMessage(x) })
		}
		if m, err := ParseX3DHInit(b); err == nil {
			same(t, m, func(x []byte) (proto.Message, error) { return ParseX3DHInit(x) })
		}
		if m, err := ParseBundleGet(b); err == nil {
			same(t, m, func(x []byte) (proto.Message, error) { return ParseBundleGet(x) })
		}
		if m, err := ParseBundle(b); err == nil {
			same(t, m, func(x []byte) (proto.Message, error) { return ParseBundle(x) })
		}
	})
}
