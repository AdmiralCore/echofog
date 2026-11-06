package chatpb

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
)

// FuzzParse covers every decoder: no panic, and what parses re-encodes to the same bytes (the
// parsers accept only canonical encodings).
func FuzzParse(f *testing.F) {
	for _, s := range samples(f) {
		f.Add(s)
	}
	f.Add(mustMarshal(f, goodInit()))
	f.Add(mustMarshal(f, goodMessage()))
	f.Add(mustMarshal(f, &BundleGet{Req: 3}))
	f.Add(mustMarshal(f, &Bundle{Req: 3, Prekeys: rep(1, 100)}))

	same := func(t *testing.T, m proto.Message, in []byte) {
		t.Helper()
		if out := mustMarshal(t, m); !bytes.Equal(out, in) {
			t.Fatalf("parsed message re-encodes differently:\n in %x\nout %x", in, out)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if m, err := ParseSecureMessage(b); err == nil {
			same(t, m, b)
		}
		if m, err := ParseX3DHInit(b); err == nil {
			same(t, m, b)
		}
		if m, err := ParseBundleGet(b); err == nil {
			same(t, m, b)
		}
		if m, err := ParseBundle(b); err == nil {
			same(t, m, b)
		}
	})
}
