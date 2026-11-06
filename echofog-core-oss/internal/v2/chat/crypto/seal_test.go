package chatcrypto

import (
	"bytes"
	"errors"
	"testing"

	"golang.org/x/crypto/curve25519"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// The vector chat2_seal: the sealing key and the box are the bytes of the spec.
func TestSealVector(t *testing.T) {
	var v map[string]string
	vector(t, "chat2_seal", &v)
	var to, ik [32]byte
	copy(to[:], hexBytes(t, v["to"]))
	copy(ik[:], hexBytes(t, v["recipient_ik"]))
	got, err := sealWith(hexBytes(t, v["eph_sk"]), to, ik, hexBytes(t, v["inner"]))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, hexBytes(t, v["SealedEnvelope"])) {
		t.Fatalf("sealed envelope differs from the vector:\n got %x\nwant %x", got, hexBytes(t, v["SealedEnvelope"]))
	}
	inner, err := openWith(hexBytes(t, v["recipient_ik_sk"]), ik, to, got)
	if err != nil || !bytes.Equal(inner, hexBytes(t, v["inner"])) {
		t.Fatalf("the vector envelope does not open: %v", err)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	clock := newClock()
	b := newUser(t, clock)
	p := b.bundle()
	var ik [32]byte
	pb := publicBundle(p)
	copy(ik[:], pb.IdentityKey[:])
	to := b.node()

	env, err := SealEnvelope(to, ik, []byte("inner bytes"))
	if err != nil {
		t.Fatal(err)
	}
	// nothing but to, eph and box is in the serialised envelope
	m, err := chatpb.ParseSealedEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := objects.Marshal(&chatpb.SealedEnvelope{To: m.To, Eph: m.Eph, Box: m.Box}); !bytes.Equal(again, env) {
		t.Fatal("the envelope carries more than to, eph and box")
	}
	if !bytes.Equal(m.To, to[:]) || len(m.Box) != len("inner bytes")+chatpb.BoxTag {
		t.Fatalf("to/box: %x %d", m.To, len(m.Box))
	}
	got, err := b.c.OpenEnvelope(to, env)
	if err != nil || string(got) != "inner bytes" {
		t.Fatalf("%q %v", got, err)
	}

	// a fresh ephemeral key each time
	env2, _ := SealEnvelope(to, ik, []byte("inner bytes"))
	m2, _ := chatpb.ParseSealedEnvelope(env2)
	if bytes.Equal(m.Eph, m2.Eph) || bytes.Equal(m.Box, m2.Box) {
		t.Fatal("two envelopes share an ephemeral key")
	}
}

func TestOpenEnvelopeRefusals(t *testing.T) {
	clock := newClock()
	b, other := newUser(t, clock), newUser(t, clock)
	var ik [32]byte
	pb := publicBundle(b.bundle())
	copy(ik[:], pb.IdentityKey[:])
	to := b.node()
	env, _ := SealEnvelope(to, ik, []byte("secret"))
	m, _ := chatpb.ParseSealedEnvelope(env)

	mut := func(f func(e *chatpb.SealedEnvelope)) []byte {
		e := &chatpb.SealedEnvelope{To: m.To, Eph: m.Eph, Box: m.Box}
		f(e)
		out, _ := objects.Marshal(e)
		return out
	}
	flip := func(x []byte) []byte { x = append([]byte(nil), x...); x[0] ^= 1; return x }
	cases := map[string][]byte{
		"garbage":            {1, 2, 3},
		"another recipient":  mut(func(e *chatpb.SealedEnvelope) { e.To = flip(e.To) }),
		"bent ephemeral key": mut(func(e *chatpb.SealedEnvelope) { e.Eph = flip(e.Eph) }),
		"bent box":           mut(func(e *chatpb.SealedEnvelope) { e.Box = flip(e.Box) }),
		"zero ephemeral key": mut(func(e *chatpb.SealedEnvelope) { e.Eph = make([]byte, 32) }),
	}
	var want string
	for name, in := range cases {
		_, err := b.c.OpenEnvelope(to, in)
		if !errors.Is(err, ErrOpen) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if want == "" {
			want = err.Error()
		} else if err.Error() != want {
			t.Errorf("%s: %q differs from %q", name, err.Error(), want)
		}
	}
	// another device cannot open it, and an envelope for another node ID is not accepted
	if _, err := other.c.OpenEnvelope(to, env); !errors.Is(err, ErrOpen) {
		t.Errorf("another device opened it: %v", err)
	}
	if _, err := b.c.OpenEnvelope(other.node(), env); !errors.Is(err, ErrOpen) {
		t.Errorf("an envelope addressed to somebody else: %v", err)
	}
	_ = curve25519.Basepoint
}
