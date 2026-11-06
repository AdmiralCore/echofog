package chatpb

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

func personalSamples(t testing.TB) map[string][]byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/v1_personal.json")
	if err != nil {
		t.Fatal(err)
	}
	var in map[string]string
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for k, v := range in {
		b, err := hex.DecodeString(v)
		if err != nil {
			t.Fatal(err)
		}
		out[k] = b
	}
	return out
}

// PersonalMessageProto keeps v1's numbers: v1 bytes decode and encode unchanged.
func TestPersonalBytesEqualV1(t *testing.T) {
	t.Parallel()
	for name, want := range personalSamples(t) {
		m := &PersonalMessageProto{}
		if err := proto.Unmarshal(want, m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := mustMarshal(t, m); !bytes.Equal(got, want) {
			t.Errorf("%s: re-encoding differs from v1:\n got %x\nwant %x", name, got, want)
		}
	}
	full := &PersonalMessageProto{Id: "m-1", SenderId: "abc", Type: PersonalMessageType_MSG_TYPE_FILE, Text: "привет", ContentHash: "h",
		Metadata: "{}", Timestamp: 1760000000123, ReplyToId: "m-0", ConvEvent: []byte{1, 2, 3}, ConvSync: []byte{4}}
	if got := mustMarshal(t, full); !bytes.Equal(got, personalSamples(t)["personal_full"]) {
		t.Errorf("v2 encoding of the full message differs from v1: %x", got)
	}
}

func TestPadPersonalReachesABucket(t *testing.T) {
	t.Parallel()
	inBucket := func(n int) bool {
		for _, b := range Buckets {
			if n == b {
				return true
			}
		}
		return false
	}
	// every text length around the bucket edges, and a spread in between
	var sizes []int
	for _, b := range Buckets[:4] {
		for d := -8; d <= 8; d++ {
			sizes = append(sizes, b+d-30)
		}
	}
	for n := 0; n < 300; n += 7 {
		sizes = append(sizes, n)
	}
	for _, n := range sizes {
		if n < 0 {
			continue
		}
		m := &PersonalMessageProto{Id: "id", Text: strings.Repeat("a", n), Timestamp: 7}
		out, err := PadPersonal(m)
		if err != nil {
			t.Fatalf("text of %d: %v", n, err)
		}
		if !inBucket(len(out)) {
			t.Fatalf("text of %d bytes gives %d, not a bucket", n, len(out))
		}
		back, err := ParsePersonal(out)
		if err != nil {
			t.Fatalf("text of %d: the padded message does not parse: %v", n, err)
		}
		if back.Text != m.Text || back.Id != m.Id || back.Timestamp != 7 {
			t.Fatalf("text of %d: content changed", n)
		}
	}
}

func TestPadPersonalSmallestBucket(t *testing.T) {
	t.Parallel()
	out, err := PadPersonal(&PersonalMessageProto{Id: "a", Text: "hi"})
	if err != nil || len(out) != 256 {
		t.Fatalf("%d %v", len(out), err)
	}
	// a pad that was in the message is replaced, not added to
	m := &PersonalMessageProto{Id: "a", Text: "hi", Pad: bytes.Repeat([]byte{1}, 500)}
	out, err = PadPersonal(m)
	if err != nil || len(out) != 256 {
		t.Fatalf("%d %v", len(out), err)
	}
}

func TestPadPersonalLargest(t *testing.T) {
	t.Parallel()
	// the largest text that still fits the last bucket, and one byte more
	fit := func(n int) error {
		_, err := PadPersonal(&PersonalMessageProto{Text: strings.Repeat("a", n)})
		return err
	}
	var last int
	for n := 57300; n < 57400; n++ {
		if fit(n) == nil {
			last = n
		}
	}
	if last < 57330 || last > 57343 {
		t.Fatalf("the largest text that fits is %d", last)
	}
	if err := fit(last + 1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("one byte more: %v", err)
	}
}

func TestParsePersonal(t *testing.T) {
	t.Parallel()
	pad := func(m *PersonalMessageProto) []byte {
		b, err := PadPersonal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	ok := func(c *Control) []byte { return pad(&PersonalMessageProto{Id: "x", Control: c}) }
	cases := []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"v1 text message", personalSamples(t)["personal_text"], true},
		{"v1 full message", personalSamples(t)["personal_full"], true},
		{"knock", ok(&Control{Kind: &Control_Knock{Knock: &Knock{Ask: true, Card: []byte{1}}}}), true},
		{"reset", ok(&Control{Kind: &Control_Reset_{Reset_: &Reset{}}}), true},
		{"read receipt", ok(&Control{Kind: &Control_Read{Read: &Receipt{Ids: [][]byte{bytes.Repeat([]byte{1}, 16)}}}}), true},
		{"delivery receipt", ok(&Control{Kind: &Control_Delivered{Delivered: &Receipt{Ids: [][]byte{bytes.Repeat([]byte{1}, 16)}}}}), true},
		{"empty control", ok(&Control{}), false},
		{"receipt id of a wrong length", ok(&Control{Kind: &Control_Read{Read: &Receipt{Ids: [][]byte{{1, 2}}}}}), false},
		{"over the largest bucket", append(pad(&PersonalMessageProto{Id: "x"}), bytes.Repeat([]byte{0x7a, 0x00}, 29000)...), false},
		{"non-canonical varint", append(pad(&PersonalMessageProto{Id: "x"}), 0x38, 0x80, 0x00), false},
		{"garbage", []byte{0xff, 0xff}, false},
	}
	for _, tc := range cases {
		m, err := ParsePersonal(tc.in)
		if tc.ok && (err != nil || m == nil) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", tc.name, err)
		}
	}
}

func goodInner() *Inner {
	return &Inner{Sender: append([]byte{1}, bytes.Repeat([]byte{7}, 32)...), Device: append([]byte{1}, bytes.Repeat([]byte{8}, 32)...),
		Id: bytes.Repeat([]byte{9}, 16), Time: 1760000000, Ratchet: []byte("secure message")}
}

func TestParseInner(t *testing.T) {
	t.Parallel()
	mod := func(f func(i *Inner)) []byte { i := goodInner(); f(i); return mustMarshal(t, i) }
	cases := []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"ratchet body", mustMarshal(t, goodInner()), true},
		{"init body", mod(func(i *Inner) { i.Ratchet, i.PqxdhInit = nil, []byte("init") }), true},
		{"no body", mod(func(i *Inner) { i.Ratchet = nil }), false},
		{"both bodies", mod(func(i *Inner) { i.PqxdhInit = []byte("x") }), false},
		{"empty ratchet body", mod(func(i *Inner) { i.Ratchet = []byte{} }), false},
		{"short sender", mod(func(i *Inner) { i.Sender = i.Sender[:32] }), false},
		{"no device", mod(func(i *Inner) { i.Device = nil }), false},
		{"short id", mod(func(i *Inner) { i.Id = i.Id[:15] }), false},
		{"garbage", []byte{0xff}, false},
	}
	for _, tc := range cases {
		m, err := ParseInner(tc.in)
		if tc.ok && (err != nil || m == nil) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", tc.name, err)
		}
	}
}

func TestParseSealedEnvelope(t *testing.T) {
	t.Parallel()
	good := &SealedEnvelope{To: bytes.Repeat([]byte{1}, 32), Eph: bytes.Repeat([]byte{2}, 32), Box: bytes.Repeat([]byte{3}, 100)}
	if m, err := ParseSealedEnvelope(mustMarshal(t, good)); err != nil || m == nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(e *SealedEnvelope){
		"short to":  func(e *SealedEnvelope) { e.To = e.To[:31] },
		"short eph": func(e *SealedEnvelope) { e.Eph = nil },
		"short box": func(e *SealedEnvelope) { e.Box = e.Box[:BoxTag-1] },
		"huge box":  func(e *SealedEnvelope) { e.Box = bytes.Repeat([]byte{1}, MaxEnvelope) },
	} {
		e := &SealedEnvelope{To: good.To, Eph: good.Eph, Box: good.Box}
		f(e)
		if _, err := ParseSealedEnvelope(mustMarshal(t, e)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The vector chat2_seal: the Inner and the SealedEnvelope encode to the bytes of the spec.
func TestEnvelopeVectorBytes(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../../../docs/protocol/v2/vectors/kdf.json")
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(all["chat2_seal"], &v); err != nil {
		t.Fatal(err)
	}
	h := func(k string) []byte {
		b, err := hex.DecodeString(v[k].(string))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	in := &Inner{}
	if err := proto.Unmarshal(h("inner"), in); err != nil {
		t.Fatal(err)
	}
	if got := mustMarshal(t, in); !bytes.Equal(got, h("inner")) {
		t.Fatalf("Inner re-encodes differently: %x", got)
	}
	env := &SealedEnvelope{To: h("to"), Eph: h("eph_pub"), Box: h("box")}
	if got := mustMarshal(t, env); !bytes.Equal(got, h("SealedEnvelope")) {
		t.Fatalf("SealedEnvelope differs from the vector: %x", got)
	}
	if _, err := ParseSealedEnvelope(h("SealedEnvelope")); err != nil {
		t.Fatal(err)
	}
}
