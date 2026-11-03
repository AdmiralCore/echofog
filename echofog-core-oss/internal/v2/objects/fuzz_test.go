package objects

import (
	"bytes"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/echofog/echofog-core/internal/v2/keys"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// Fuzz targets for every decoder and verifier. Seeds come from the vectors. Properties: no panic;
// whatever verifies keeps the exact input bytes; a canonical body parses and re-encodes to itself
// when its unknown fields come after the known ones.

func seedEnvelopes(f *testing.F) {
	so := loadVector(f, "signed-object.json")
	for _, o := range get(f, so, "objects").([]any) {
		f.Add(unhex(f, o.(map[string]any)["envelope"].(string)))
	}
	nc := loadVector(f, "node-card.json")
	f.Add(hx(f, nc, "device_list", "envelope"))
	for _, s := range []string{"public", "peer", "invite"} {
		f.Add(hx(f, nc, s, "envelope"))
	}
	neg := loadVector(f, "negative.json")
	f.Add(hx(f, neg, "signature_s_not_canonical", "envelope"))
	f.Add(hx(f, neg, "small_order_signer", "envelope"))
}

func fuzzNow(off uint16) time.Time { return time.Unix(int64(fixedTime)+int64(off)*60, 0) }

func FuzzParseEnvelope(f *testing.F) {
	seedEnvelopes(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := Parse(b)
		if err != nil {
			return
		}
		if !bytes.Equal(e.Raw, b) || len(e.SignedBytes())+len(e.Sig) != len(b) {
			t.Fatal("parse does not cover the input")
		}
		// The parsed fields rebuild the same prefix.
		pre := []byte{FormatV2, byte(len(e.Label))}
		pre = append(pre, e.Label...)
		pre = append(pre, e.Signer...)
		pre = append(pre, b[len(pre):len(pre)+16]...)
		pre = appendVarint(pre, uint64(len(e.Body)))
		pre = append(pre, e.Body...)
		if !bytes.Equal(pre, e.SignedBytes()) {
			t.Fatal("fields do not rebuild the signed prefix")
		}
	})
}

func FuzzVerify(f *testing.F) {
	seedEnvelopes(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := Verify(b, vectorNow)
		if err != nil {
			return
		}
		if !bytes.Equal(e.Raw, b) {
			t.Fatal("verified object not kept verbatim")
		}
		_ = e.StoreExpiry(vectorNow)
	})
}

func FuzzVerifyCard(f *testing.F) {
	seedEnvelopes(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := VerifyCard(b, vectorNow)
		if err != nil {
			return
		}
		if !bytes.Equal(c.Env.Raw, b) || c.NodeID != c.Account.ID() || c.DeviceID != c.DeviceKey.ID() {
			t.Fatal("card identity")
		}
		if !c.Devices.Contains(c.DeviceKey) {
			t.Fatal("card device not in its list")
		}
		if c.Scope == ScopePublic {
			for _, a := range c.Body.GetAddrs() {
				if a.GetSuite() == SuiteNoise3 {
					t.Fatal("noise3 in a public card")
				}
			}
		}
		_, _, _, _ = c.Addrs(), c.MaskKeys(), c.Features(), c.Shop()
		book := NewCardBook()
		_, _ = book.Add(c)
	})
}

// FuzzCardBody fuzzes the body under a valid signature, so the card rules are reached.
func FuzzCardBody(f *testing.F) {
	cv := loadCardVector(f)
	for _, s := range []string{"public", "peer", "invite"} {
		e, err := Parse(hx(f, cv.v, s, "envelope"))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(append([]byte(nil), e.Body...))
	}
	neg := loadVector(f, "negative.json")
	for _, o := range get(f, neg, "non_canonical_bodies").([]any) {
		f.Add(unhex(f, o.(map[string]any)["body"].(string)))
	}
	md := (&pb.NodeCard{}).ProtoReflect().Descriptor()
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 3800 {
			return
		}
		canon := CheckCanonical(body, md) == nil
		env := sealRaw(LabelCard, cv.device, fixedTime, fixedTime+day, body)
		c, err := VerifyCard(env, vectorNow)
		if err == nil && !canon {
			t.Fatal("non-canonical body accepted")
		}
		if err == nil && !bytes.Equal(c.Env.Body, body) {
			t.Fatal("body not kept")
		}
	})
}

// FuzzCanonical checks the canonical checker against protobuf-go: whatever it accepts parses, and
// a canonical encoding of a parsed message is accepted.
func FuzzCanonical(f *testing.F) {
	cv := loadCardVector(f)
	e, _ := Parse(hx(f, cv.v, "public", "envelope"))
	f.Add(append([]byte(nil), e.Body...))
	f.Add([]byte{0x08, 0xc8, 0x01})
	md := (&pb.NodeCard{}).ProtoReflect().Descriptor()
	f.Fuzz(func(t *testing.T, b []byte) {
		m := &pb.NodeCard{}
		perr := proto.Unmarshal(b, m)
		if CheckCanonical(b, md) == nil && perr != nil {
			t.Fatalf("canonical but unparsable: %v", perr)
		}
		if perr != nil {
			return
		}
		clearUnknown(m.ProtoReflect())
		re, err := Marshal(m)
		if err != nil {
			t.Fatalf("re-encoding of a parsed message is not canonical: %v", err)
		}
		m2 := &pb.NodeCard{}
		if err := proto.Unmarshal(re, m2); err != nil || !proto.Equal(m, m2) {
			t.Fatal("canonical re-encoding changes the message")
		}
		re2, _ := Marshal(m2)
		if !bytes.Equal(re, re2) {
			t.Fatal("canonical encoding not stable")
		}
	})
}

func FuzzVerifyDeviceList(f *testing.F) {
	seedEnvelopes(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		dl, err := VerifyDeviceList(b, vectorNow)
		if err != nil {
			return
		}
		for _, d := range dl.Devices() {
			_ = dl.Contains(d.GetKey())
		}
	})
}

func FuzzVerifyOthers(f *testing.F) {
	seedEnvelopes(f)
	s := testSigner(f, "fuzz-others")
	for _, label := range []string{LabelPrekeys, LabelMailboxHint, LabelName} {
		b, _ := Seal(label, s, 1, fixedTime+hour, nil)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = VerifyPrekeys(b, vectorNow)
		_, _ = VerifyMailboxHint(b, vectorNow)
		_, _ = VerifyNameBinding(b, vectorNow)
	})
}

// FuzzReleaseBody fuzzes the release object body under valid threshold-capable signatures.
func FuzzReleaseBody(f *testing.F) {
	so := loadVector(f, "signed-object.json")
	o := get(f, so, "objects").([]any)[0].(map[string]any)
	f.Add(unhex(f, o["body"].(string)), uint16(0))
	f.Add([]byte{}, uint16(1))
	first := signer(f, str(f, so, "signer_seed"))
	second := signer(f, str(f, so, "seeds", "release/second_key"))
	f.Fuzz(func(t *testing.T, body []byte, off uint16) {
		if len(body) > 20000 {
			return
		}
		st, err := NewReleaseState([]keys.AKey{first.Public(), second.Public(), testSigner(t, "third").Public()})
		if err != nil {
			t.Fatal(err)
		}
		env := sealRaw(LabelRelease, first, fixedTime, fixedTime+30*day, body)
		m, err := st.Apply(env, fuzzNow(off))
		if err != nil {
			return
		}
		if len(m.Signers) < 2 || len(m.Keys) < 2 || len(m.Keys) > 5 {
			t.Fatal("threshold or key-set bounds broken")
		}
		for _, c := range []string{"proto", "svc:3"} {
			_ = st.PeerRefused(c, 200, 100, fuzzNow(off))
			if st.PeerRefused(c, 200, 200, fuzzNow(off)) {
				t.Fatal("refused the node's own version")
			}
		}
	})
}

// clearUnknown drops unknown fields everywhere: protobuf-go keeps them in received order, which a
// canonical re-encoding does not promise for arbitrary input.
func clearUnknown(m protoreflect.Message) {
	m.SetUnknown(nil)
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		if fd.IsList() {
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				clearUnknown(l.Get(i).Message())
			}
			return true
		}
		clearUnknown(v.Message())
		return true
	})
}

func FuzzParseLink(f *testing.F) {
	lv := loadVector(f, "link.json")
	f.Add(str(f, lv, "contact", "link"))
	f.Add(str(f, lv, "invitation", "link"))
	f.Add(str(f, lv, "invitation", "web_form"))
	f.Add("echofog://join?v=3")
	f.Fuzz(func(t *testing.T, s string) {
		l, err := ParseLink(s)
		if err != nil {
			return
		}
		if len(l.Card) == 0 || len(l.Card) > 4096 || (l.Secret != nil && len(l.Secret) != 16) {
			t.Fatal("link bounds")
		}
		enc, err := EncodeLink(l.Card, l.Secret)
		if err != nil {
			t.Fatal(err)
		}
		back, err := ParseLink(enc)
		if err != nil || !bytes.Equal(back.Card, l.Card) || !bytes.Equal(back.Secret, l.Secret) {
			t.Fatalf("re-encoded link: %v", err)
		}
		_, _ = l.Verify(vectorNow)
	})
}
