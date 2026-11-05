package cards

import (
	"bytes"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

func TestIssuedEncodingRoundTrip(t *testing.T) {
	in := map[uint64]issuedRecord{
		7:  {Scope: objects.ScopePeer, At: 1_800_000_000},
		9:  {Scope: objects.ScopeInvite, At: 1_800_000_100},
		11: {Scope: objects.ScopePublic, At: 1_800_000_200},
	}
	b, err := encodeIssued(in)
	if err != nil {
		t.Fatalf("encodeIssued: %v", err)
	}
	out, err := decodeIssued(b)
	if err != nil {
		t.Fatalf("decodeIssued: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("decoded %d records, want %d", len(out), len(in))
	}
	for seq, want := range in {
		got, ok := out[seq]
		if !ok {
			t.Fatalf("seq %d lost", seq)
		}
		if got != want {
			t.Fatalf("seq %d = %+v, want %+v", seq, got, want)
		}
	}
}

func TestIssuedDecodingRejectsMalformed(t *testing.T) {
	good, err := encodeIssued(map[uint64]issuedRecord{1: {Scope: objects.ScopePeer, At: 1}})
	if err != nil {
		t.Fatalf("encodeIssued: %v", err)
	}
	cases := map[string][]byte{
		"empty":       nil,
		"bad version": append([]byte{2}, good[1:]...),
		"truncated":   good[:len(good)-1],
		"long count":  {1, 0xff, 0xff, 0xff, 0xff},
		"trailing":    append(append([]byte(nil), good...), 0),
	}
	for name, b := range cases {
		b := b
		t.Run(name, func(t *testing.T) {
			if _, err := decodeIssued(b); err == nil {
				t.Fatalf("decoded malformed input")
			}
		})
	}
}

func TestCurrentEncodingRoundTrip(t *testing.T) {
	fp := [32]byte{1, 2, 3}
	env := []byte{2, 1, 'a'}
	b := encodeCurrent(fp, env)
	gotFP, gotEnv, err := decodeCurrent(b)
	if err != nil {
		t.Fatalf("decodeCurrent: %v", err)
	}
	if gotFP != fp || !bytes.Equal(gotEnv, env) {
		t.Fatalf("decoded current record differs")
	}
	for _, bad := range [][]byte{nil, {1}, append([]byte{1}, fp[:]...), append([]byte{7}, b[1:]...)} {
		if _, _, err := decodeCurrent(bad); err == nil {
			t.Fatalf("decoded malformed current record")
		}
	}
}

func FuzzDecodeIssued(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 0})
	f.Add([]byte{1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = decodeIssued(b)
	})
}

func FuzzDecodeCurrent(f *testing.F) {
	f.Add(encodeCurrent([32]byte{}, []byte{1, 2, 3}))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = decodeCurrent(b)
	})
}
