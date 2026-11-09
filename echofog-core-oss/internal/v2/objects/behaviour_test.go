package objects

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/echofog/echofog-core/internal/v2/keys"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// ---------------------------------------------------------------------------------------------
// Fixture: one account with one device, cards built from the vector's public card.

type fixture struct {
	cv      cardVector
	account *keys.Signer
	device  *keys.Signer
	devEnv  []byte
}

func newFixture(t testing.TB) fixture {
	cv := loadCardVector(t)
	return fixture{cv: cv, account: cv.account, device: cv.device, devEnv: cv.devEnv}
}

// sealBody encodes and signs a card body without the SealCard rule check.
func sealBody(t testing.TB, s *keys.Signer, seq, notAfter uint64, c *pb.NodeCard) []byte {
	t.Helper()
	b, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	env, err := Seal(LabelCard, s, seq, notAfter, b)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func (f fixture) public(t testing.TB) *pb.NodeCard { return f.cv.publicCard(t) }

func (f fixture) seal(t testing.TB, c *pb.NodeCard) []byte {
	return sealBody(t, f.device, fixedTime, fixedTime+6*day, c)
}

func mustInvalid(t testing.TB, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: accepted", what)
	}
	if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("%s: unexpected error %v", what, err)
	}
}

// ---------------------------------------------------------------------------------------------
// Device list membership (D35)

func TestCardDeviceMustBeInAccountList(t *testing.T) {
	f := newFixture(t)
	other := testSigner(t, "other-device")

	// A list signed by the account that lists another device.
	list, err := SealDeviceList(f.account, fixedTime, fixedTime+30*day,
		&pb.DeviceList{Devices: []*pb.Device{{Key: other.Public(), Added: fixedTime, Flags: DevicePrimary}}})
	if err != nil {
		t.Fatal(err)
	}
	c := f.public(t)
	c.Devices = list
	_, err = VerifyCard(f.seal(t, c), vectorNow)
	mustInvalid(t, err, "device not in list")

	// The same card signed by the listed device is valid.
	if _, err := VerifyCard(sealBody(t, other, fixedTime, fixedTime+6*day, c), vectorNow); err != nil {
		t.Fatalf("listed device: %v", err)
	}

	// A list that names the device but is signed by another key than the card's account.
	stranger := testSigner(t, "stranger-account")
	list2, err := SealDeviceList(stranger, fixedTime, fixedTime+30*day,
		&pb.DeviceList{Devices: []*pb.Device{{Key: f.device.Public(), Added: fixedTime, Flags: DevicePrimary}}})
	if err != nil {
		t.Fatal(err)
	}
	c = f.public(t)
	c.Devices = list2
	_, err = VerifyCard(f.seal(t, c), vectorNow)
	mustInvalid(t, err, "list by another account")

	// Account = stranger with its list: valid, and the node ID follows the account.
	c.Account = stranger.Public()
	card, err := VerifyCard(f.seal(t, c), vectorNow)
	if err != nil {
		t.Fatal(err)
	}
	if card.NodeID != stranger.Public().ID() {
		t.Fatal("node ID is not SHA-256(account)")
	}

	// Missing list, list of another label, expired list.
	c = f.public(t)
	c.Devices = nil
	_, err = VerifyCard(f.seal(t, c), vectorNow)
	mustInvalid(t, err, "missing devices")
	c.Devices = f.seal(t, f.public(t))
	_, err = VerifyCard(f.seal(t, c), vectorNow)
	mustInvalid(t, err, "devices of another label")
	c = f.public(t)
	_, err = VerifyCard(sealBody(t, f.device, fixedTime, fixedTime+31*day, c), time.Unix(int64(fixedTime+30*day+61), 0))
	mustInvalid(t, err, "expired device list")
	// Missing account.
	c = f.public(t)
	c.Account = nil
	_, err = VerifyCard(f.seal(t, c), vectorNow)
	mustInvalid(t, err, "missing account")
}

// ---------------------------------------------------------------------------------------------
// Scopes

func TestCardScopeRules(t *testing.T) {
	f := newFixture(t)
	noise3 := &pb.Addr{Suite: SuiteNoise3, Host: "192.0.2.10", Port: 443, Mask: 2, Params: params(t, "", 1)}
	maskKey := &pb.MaskKey{Key: keys.New(keys.X25519, f.cv.mask3), NotAfter: fixedTime + 7*day}

	c := f.public(t)
	c.Addrs = append([]*pb.Addr{noise3}, c.Addrs...)
	c.MaskKeys = append(c.MaskKeys, maskKey)
	_, err := VerifyCard(f.seal(t, c), vectorNow)
	mustInvalid(t, err, "noise3 address in a public card")

	// The same content at peer scope is valid but not publishable.
	c.Scope = uint32(ScopePeer)
	card, err := VerifyCard(f.seal(t, c), vectorNow)
	if err != nil {
		t.Fatalf("peer card: %v", err)
	}
	if card.Publishable() {
		t.Fatal("peer card publishable")
	}

	// A public card may not carry a masking key that no address uses (for example a noise3 key).
	c = f.public(t)
	c.MaskKeys = append(c.MaskKeys, maskKey)
	_, err = VerifyCard(f.seal(t, c), vectorNow)
	mustInvalid(t, err, "unused (noise3) masking key in a public card")

	for _, flag := range []uint32{FlagUnlisted, FlagInviteScoped} {
		c = f.public(t)
		c.Flags |= flag
		_, err = VerifyCard(f.seal(t, c), vectorNow)
		mustInvalid(t, err, "public card with flag")
	}

	// Invite scope needs an 8-byte invite_id and flags unlisted|invite-scoped; invite_id only there.
	c = f.cv.inviteCard(t)
	c.InviteId = nil
	_, err = VerifyCard(f.seal(t, c), vectorNow)
	mustInvalid(t, err, "invite without invite_id")
	c = f.cv.inviteCard(t)
	c.Flags &^= FlagUnlisted
	_, err = VerifyCard(f.seal(t, c), vectorNow)
	mustInvalid(t, err, "invite without unlisted")
	c = f.public(t)
	c.InviteId = []byte("12345678")
	_, err = VerifyCard(f.seal(t, c), vectorNow)
	mustInvalid(t, err, "invite_id in a public card")

	// The public vector card is publishable; an unknown scope verifies but is never forwarded.
	card, err = VerifyCard(f.seal(t, f.public(t)), vectorNow)
	if err != nil || !card.Publishable() {
		t.Fatalf("public card: %v", err)
	}
	c = f.public(t)
	c.Scope = 7
	card, err = VerifyCard(f.seal(t, c), vectorNow)
	if err != nil || card.Publishable() {
		t.Fatalf("unknown scope: %v publishable=%v", err, card != nil && card.Publishable())
	}

	// Lifetime: 30 days only for invite cards.
	c = f.public(t)
	c.Scope = uint32(ScopePeer)
	if _, err := VerifyCard(sealBody(t, f.device, fixedTime, fixedTime+8*day, c), vectorNow); !errors.Is(err, ErrLifetime) {
		t.Fatalf("8-day peer card: %v", err)
	}
	if _, err := VerifyCard(sealBody(t, f.device, fixedTime, fixedTime+30*day, f.cv.inviteCard(t)), vectorNow); err != nil {
		t.Fatalf("30-day invite card: %v", err)
	}
}

func TestCardAddressRules(t *testing.T) {
	f := newFixture(t)
	cases := map[string]func(c *pb.NodeCard){
		"mask index out of range": func(c *pb.NodeCard) { c.Addrs[0].Mask = 2 },
		"v2 suite without mask":   func(c *pb.NodeCard) { c.Addrs[0].Mask = 0 },
		"port out of range":       func(c *pb.NodeCard) { c.Addrs[0].Port = 70000 },
		"no host":                 func(c *pb.NodeCard) { c.Addrs[0].Host = "" },
		"host too long":           func(c *pb.NodeCard) { c.Addrs[0].Host = strings.Repeat("a", 254) },
		"params not canonical":    func(c *pb.NodeCard) { c.Addrs[0].Params = []byte{0x12, 0x00, 0x0a, 0x01, 0x61} },
		"mask key not x25519":     func(c *pb.NodeCard) { c.MaskKeys[0].Key = f.account.Public() },
		"mask key outlives card":  func(c *pb.NodeCard) { c.MaskKeys[0].NotAfter = fixedTime + 13*day + 1 },
		"hs_key missing":          func(c *pb.NodeCard) { c.HsKey = nil },
		"proto_ver below 200":     func(c *pb.NodeCard) { c.ProtoVer = 199 },
		"server without camouflaged address": func(c *pb.NodeCard) {
			c.Addrs = []*pb.Addr{{Suite: 99, Host: "192.0.2.1", Port: 1}}
			c.MaskKeys = nil
		},
		"ws2 without wss url": func(c *pb.NodeCard) {
			c.Addrs = append(c.Addrs, &pb.Addr{Suite: SuiteWS2, Url: "https://cdn.example.org/x", Mask: 1})
		},
		"flag exit without offer": func(c *pb.NodeCard) { c.Exit = nil },
		"exit region lower case":  func(c *pb.NodeCard) { c.Exit.Region = "nl" },
		"release too long":        func(c *pb.NodeCard) { c.Release = strings.Repeat("1", 33) },
		"prekeys_hash length":     func(c *pb.NodeCard) { c.PrekeysHash = []byte{1, 2, 3} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			c := f.public(t)
			mut(c)
			_, err := VerifyCard(f.seal(t, c), vectorNow)
			mustInvalid(t, err, name)
		})
	}

	// Allowed variations.
	ok := map[string]func(c *pb.NodeCard){
		"peer server without camouflage": func(c *pb.NodeCard) {
			c.Addrs = []*pb.Addr{{Suite: SuiteNoise3, Host: "192.0.2.1", Port: 1, Mask: 1}}
			c.Scope = uint32(ScopePeer)
		},
		"domestic-only server without camouflage": func(c *pb.NodeCard) {
			c.Addrs = []*pb.Addr{{Suite: 99, Host: "192.0.2.1", Port: 1}}
			c.MaskKeys = nil
			c.Flags |= FlagDomesticOnly
		},
		"user without addresses":          func(c *pb.NodeCard) { c.Role = RoleUser; c.Addrs = nil; c.MaskKeys = nil },
		"free exit (empty offer present)": func(c *pb.NodeCard) { c.Exit = &pb.ExitOffer{} },
		"offer without flag (not live)":   func(c *pb.NodeCard) { c.Flags &^= FlagExit },
		"unknown role is a user":          func(c *pb.NodeCard) { c.Role = 9 },
		"unknown suite without mask":      func(c *pb.NodeCard) { c.Addrs = append(c.Addrs, &pb.Addr{Suite: 0x77}) },
	}
	for name, mut := range ok {
		t.Run(name, func(t *testing.T) {
			c := f.public(t)
			mut(c)
			card, err := VerifyCard(f.seal(t, c), vectorNow)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if name == "unknown role is a user" && card.Role() != RoleUser {
				t.Fatal("unknown role not treated as user")
			}
			if name == "offer without flag (not live)" && card.ExitOffer() != nil {
				t.Fatal("offer without flag reported live")
			}
			if name == "free exit (empty offer present)" && card.ExitOffer() == nil {
				t.Fatal("empty offer lost")
			}
		})
	}
}

func TestCardProcessingLimits(t *testing.T) {
	f := newFixture(t)
	c := f.public(t)
	for i := 0; i < 70; i++ {
		c.Features = append(c.Features, uint32(100+i))
	}
	for i := 0; i < 20; i++ {
		c.Addrs = append(c.Addrs, &pb.Addr{Suite: SuiteReality2TCP, Host: "198.51.100.7", Port: uint32(1000 + i), Mask: 1})
	}
	c.Shop = append(c.Shop, "https://a.example/1", "https://a.example/2", "https://a.example/3")
	card, err := VerifyCard(f.seal(t, c), vectorNow)
	if err != nil {
		t.Fatalf("count limits are processing limits, not validity limits: %v", err)
	}
	if len(card.Features()) != 64 || len(card.Addrs()) != 16 || len(card.Shop()) != 3 {
		t.Fatalf("processed: %d features, %d addrs, %d shop", len(card.Features()), len(card.Addrs()), len(card.Shop()))
	}
	// The size limit is a validity limit.
	c = f.public(t)
	for i := 0; i < 200; i++ {
		c.Addrs = append(c.Addrs, &pb.Addr{Suite: SuiteReality2TCP, Host: "198.51.100.7", Port: uint32(1000 + i), Mask: 1})
	}
	b, _ := Marshal(c)
	env := sealRaw(LabelCard, f.device, fixedTime, fixedTime+day, b)
	if _, err := VerifyCard(env, vectorNow); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized card: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// Unknown fields from a future version

func appendField(b []byte, num protowire.Number, wt protowire.Type, v []byte) []byte {
	b = protowire.AppendTag(b, num, wt)
	switch wt {
	case protowire.BytesType:
		b = protowire.AppendBytes(b, v)
	default:
		b = append(b, v...)
	}
	return b
}

func TestCardFutureVersionFields(t *testing.T) {
	f := newFixture(t)
	c := f.public(t)
	// Nested unknown fields: Addr field 8, ExitOffer field 9, MaskKey field 3.
	c.Addrs[0].ProtoReflect().SetUnknown(appendField(nil, 8, protowire.VarintType, protowire.AppendVarint(nil, 42)))
	c.Exit.ProtoReflect().SetUnknown(appendField(nil, 9, protowire.BytesType, []byte("future")))
	c.MaskKeys[0].ProtoReflect().SetUnknown(appendField(nil, 3, protowire.Fixed32Type, []byte{1, 2, 3, 4}))
	// Unknown flag bits, a future feature and a future service.
	c.Flags |= 1 << 20
	c.Features = append(c.Features, 0x0099)
	c.Services = append(c.Services, &pb.ServiceOffer{Id: 11, Versions: []uint32{1}})
	body, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	// Top-level unknown fields after field 19 (the cluster reference), one of them repeated.
	body = appendField(body, 20, protowire.BytesType, []byte("new field"))
	body = appendField(body, 21, protowire.VarintType, protowire.AppendVarint(nil, 7))
	body = appendField(body, 22, protowire.BytesType, []byte("a"))
	body = appendField(body, 22, protowire.BytesType, []byte("b"))
	body = appendField(body, 23, protowire.Fixed64Type, []byte{1, 2, 3, 4, 5, 6, 7, 8})

	env, err := Seal(LabelCard, f.device, fixedTime, fixedTime+day, body)
	if err != nil {
		t.Fatal(err)
	}
	card, err := VerifyCard(env, vectorNow)
	if err != nil {
		t.Fatalf("future card: %v", err)
	}
	if !bytes.Equal(card.Env.Raw, env) || !bytes.Equal(card.Env.Body, body) {
		t.Fatal("received bytes not kept")
	}
	if len(card.Body.ProtoReflect().GetUnknown()) == 0 || len(card.Body.GetAddrs()[0].ProtoReflect().GetUnknown()) == 0 {
		t.Fatal("unknown fields dropped by the parser")
	}
	// Re-encoding the parsed body gives the same bytes: unknown fields are kept.
	re, err := Marshal(card.Body)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	eqHex(t, "re-encoded body", re, body)

	// What a future field may not do.
	bad := map[string][]byte{
		"unknown field before a known one": appendField(appendField(nil, 20, protowire.VarintType, []byte{1}), 1, protowire.VarintType, []byte{0x01}),
		"unknown group":                    append(append([]byte(nil), body...), protowire.AppendTag(nil, 30, protowire.StartGroupType)...),
		"unknown non-minimal varint":       append(append([]byte(nil), body...), 0xc0, 0x01, 0x81, 0x00), // field 24 wt0, value 1 in 2 bytes
		"unknown non-minimal tag":          append(append([]byte(nil), body...), 0xc8, 0x81, 0x00, 0x01), // field 25 tag in 3 bytes
		"truncated unknown field":          append(append([]byte(nil), body...), 0xc2, 0x01, 0x05, 0x01),
	}
	for name, b := range bad {
		env := sealRaw(LabelCard, f.device, fixedTime, fixedTime+day, b)
		if _, err := VerifyCard(env, vectorNow); !errors.Is(err, ErrNonCanonical) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestCanonicalRules(t *testing.T) {
	md := (&pb.NodeCard{}).ProtoReflect().Descriptor()
	bad := map[string]string{
		"scalar default present":         "0800",
		"empty bytes present":            "08c8011200",
		"empty packed field":             "08c8011a00",
		"uint32 out of range":            "0880808080 10",
		"packed element non-minimal":     "08c8011a028100",
		"embedded message not canonical": "08c8015202" + "1000",
		"wrong wire type for known":      "0dc8000000",
		"field number zero":              "0001",
		"invalid utf-8 string":           "08c8016201ff",
		"truncated":                      "08",
	}
	for name, h := range bad {
		b := unhex(t, strings.ReplaceAll(h, " ", ""))
		if err := CheckCanonical(b, md); !errors.Is(err, ErrNonCanonical) {
			t.Errorf("%s (%x): %v", name, b, err)
		}
	}
	good := map[string]string{
		"empty message present (free exit)": "08c8015200",
		"repeated message elements":         "08c80122002200",
		"empty element of repeated string":  "08c8015a005a00",
	}
	for name, h := range good {
		if err := CheckCanonical(unhex(t, h), md); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Marshal of a message with defaults produces the empty body.
	b, err := Marshal(&pb.NodeCard{})
	if err != nil || len(b) != 0 {
		t.Fatalf("empty card body: %x %v", b, err)
	}
	// Deep nesting through unknown messages is not followed; through known ones it is bounded.
	if err := CheckCanonical(nil, md); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------------------------
// Envelope structure, algorithms, sizes, expiry

func TestEnvelopeStructure(t *testing.T) {
	s := testSigner(t, "env")
	good, err := Seal("echofog/test/9", s, 1, fixedTime+day, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(good, vectorNow); err != nil {
		t.Fatalf("unknown-label object: %v", err)
	}

	mut := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	cases := map[string]struct {
		b   []byte
		err error
	}{
		"format 0x03":     {mut(func(b []byte) []byte { b[0] = 3; return b }), ErrMalformed},
		"label length 0":  {mut(func(b []byte) []byte { b[1] = 0; return b }), ErrMalformed},
		"label not ascii": {mut(func(b []byte) []byte { b[2] = ' '; return b }), ErrMalformed},
		"unknown alg":     {mut(func(b []byte) []byte { b[2+14] = 0x7a; return b }), ErrUnusable},
		"reserved hybrid": {mut(func(b []byte) []byte { b[2+14] = 0x11; return b }), ErrUnusable},
		"x25519 signer":   {mut(func(b []byte) []byte { b[2+14] = 0x02; return b }), ErrUnusable},
		"trailing byte":   {append(append([]byte(nil), good...), 0), ErrMalformed},
		"truncated":       {good[:len(good)-1], ErrMalformed},
		"flipped sig bit": {mut(func(b []byte) []byte { b[len(b)-1] ^= 1; return b }), ErrBadSignature},
		"flipped seq bit": {mut(func(b []byte) []byte { b[2+14+33+6] ^= 1; return b }), ErrBadSignature},
		"empty":           {nil, ErrMalformed},
	}
	for name, c := range cases {
		if _, err := Verify(c.b, vectorNow); !errors.Is(err, c.err) {
			t.Errorf("%s: %v, want %v", name, err, c.err)
		}
	}
	if _, err := Seal("x", s, 0, 1, nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("seq 0 sealed: %v", err)
	}
	if env := sealRaw("x", s, 0, fixedTime+day, nil); true {
		if _, err := Verify(env, vectorNow); !errors.Is(err, ErrMalformed) {
			t.Errorf("seq 0 accepted: %v", err)
		}
	}
	// A non-minimal body_len.
	raw := sealRaw("echofog/test/9", s, 1, fixedTime+day, nil)
	nm := append(append(append([]byte(nil), raw[:2+14+33+16]...), 0x40, 0x00), raw[2+14+33+16+1:]...)
	if _, err := Verify(nm, vectorNow); !errors.Is(err, ErrMalformed) {
		t.Errorf("non-minimal body_len: %v", err)
	}

	// Size limits: the generic 2048 for unknown labels, the label's limits for known ones.
	big := appendField(nil, 1, protowire.BytesType, make([]byte, 2000))
	if _, err := Verify(sealRaw("echofog/test/9", s, 1, fixedTime+day, big), vectorNow); !errors.Is(err, ErrTooLarge) {
		t.Errorf("unknown label over 2048: %v", err)
	}
	// echofog/name/2: body ≤ 512, envelope ≤ 768.
	if _, err := Verify(sealRaw(LabelName, s, 1, fixedTime+hour, appendField(nil, 1, protowire.BytesType, make([]byte, 600))), vectorNow); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized name binding: %v", err)
	}
	// The algorithm is checked before the size: an oversized object with an unknown algorithm is
	// unusable, not too large.
	over := sealRaw("echofog/test/9", s, 1, fixedTime+day, big)
	over[2+14] = 0x7a
	if _, err := Verify(over, vectorNow); !errors.Is(err, ErrUnusable) {
		t.Errorf("unknown alg, oversized: %v", err)
	}
	// The generic limit applies to unknown labels of any prefix.
	if _, err := Verify(sealRaw("other/label/1", s, 1, fixedTime+day, big), vectorNow); !errors.Is(err, ErrTooLarge) {
		t.Errorf("non-echofog label over 2048: %v", err)
	}
	// Body of an unknown label still follows the schema-free rules.
	if _, err := Verify(sealRaw("echofog/test/9", s, 1, fixedTime+day, []byte{0x10, 0x01, 0x08, 0x01}), vectorNow); !errors.Is(err, ErrNonCanonical) {
		t.Errorf("unknown label body out of order: %v", err)
	}
	// A wrong label for the typed verifier.
	if _, err := VerifyCard(good, vectorNow); !errors.Is(err, ErrWrongLabel) {
		t.Errorf("VerifyCard on another label: %v", err)
	}
}

func TestExpiryAndClockTolerance(t *testing.T) {
	s := testSigner(t, "clock")
	at := func(off int64) time.Time { return time.Unix(int64(fixedTime)+off, 0) }
	env := func(label string, notAfter uint64) []byte {
		b, err := Seal(label, s, 1, notAfter, nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	e := env(LabelDevices, fixedTime)
	// Valid while not_after > now − 60.
	for off, want := range map[int64]error{-1000: nil, 0: nil, 59: nil, 60: ErrExpired, 61: ErrExpired} {
		if _, err := Verify(e, at(off)); !errors.Is(err, want) && !(want == nil && err == nil) {
			t.Errorf("now = not_after + %d: %v, want %v", off, err, want)
		}
	}
	// Lifetime ≤ max + 60.
	for label, max := range map[string]uint64{LabelDevices: 30 * day, LabelMailboxHint: 6 * hour,
		LabelPrekeys: 30 * day, LabelName: 30 * day, LabelRelease: 400 * day, LabelCard: 30 * day} {
		if _, err := Verify(env(label, fixedTime+max+60), vectorNow); errors.Is(err, ErrLifetime) || errors.Is(err, ErrExpired) {
			t.Errorf("%s at max+60: %v", label, err)
		}
		if _, err := Verify(env(label, fixedTime+max+61), vectorNow); !errors.Is(err, ErrLifetime) {
			t.Errorf("%s at max+61: %v", label, err)
		}
	}
	// Card: 7 days for public and peer cards (+60 s).
	f := newFixture(t)
	if _, err := VerifyCard(sealBody(t, f.device, fixedTime, fixedTime+7*day+60, f.public(t)), vectorNow); err != nil {
		t.Errorf("card at 7 days + 60 s: %v", err)
	}
	if _, err := VerifyCard(sealBody(t, f.device, fixedTime, fixedTime+7*day+61, f.public(t)), vectorNow); !errors.Is(err, ErrLifetime) {
		t.Errorf("card at 7 days + 61 s: %v", err)
	}
	// A card is valid until its own expiry even if the clock is ahead by less than 60 s.
	if _, err := VerifyCard(sealBody(t, f.device, fixedTime, fixedTime+day, f.public(t)), at(day+59)); err != nil {
		t.Errorf("card 59 s past not_after: %v", err)
	}
	// Unknown label: no lifetime limit, stored until min(not_after, receipt + 24 h).
	u := env("echofog/future/3", fixedTime+1000*day)
	ue, err := Verify(u, vectorNow)
	if err != nil {
		t.Fatalf("unknown label, long lifetime: %v", err)
	}
	if got := ue.StoreExpiry(vectorNow); got != fixedTime+day {
		t.Errorf("unknown label StoreExpiry %d", got)
	}
	short, _ := Verify(env("echofog/future/3", fixedTime+hour), vectorNow)
	if got := short.StoreExpiry(vectorNow); got != fixedTime+hour {
		t.Errorf("short unknown label StoreExpiry %d", got)
	}
	k, _ := Verify(env(LabelDevices, fixedTime+20*day), vectorNow)
	if got := k.StoreExpiry(vectorNow); got != fixedTime+20*day {
		t.Errorf("known label StoreExpiry %d", got)
	}
}

func TestEquivocation(t *testing.T) {
	s := testSigner(t, "equivocation")
	a, _ := Seal(LabelDevices, s, 5, fixedTime+day, nil)
	b, _ := Seal(LabelDevices, s, 5, fixedTime+2*day, nil)
	c, _ := Seal(LabelDevices, s, 6, fixedTime+day, nil)
	ea, _ := Verify(a, vectorNow)
	eb, _ := Verify(b, vectorNow)
	ec, _ := Verify(c, vectorNow)
	if !Equivocation(ea, eb) || Equivocation(ea, ec) || Equivocation(ea, ea) {
		t.Fatal("Equivocation")
	}
	ia, ib := ea.ID(), eb.ID()
	want := ea
	if bytes.Compare(ib[:], ia[:]) < 0 {
		want = eb
	}
	if Prefer(ea, eb) != want || Prefer(eb, ea) != want {
		t.Fatal("equivocation must keep the smaller object_id, whatever the order")
	}
	if Prefer(ea, ec) != ec || Prefer(ec, eb) != ec {
		t.Fatal("higher seq wins")
	}
}

// ---------------------------------------------------------------------------------------------
// Shop links (D17)

func TestShopLinks(t *testing.T) {
	ok := []string{
		"https://shop.example.org/echofog",
		"https://t.me/some_operator",
		"https://xn--80ak6aa92e.com/buy?plan=1",
		"https://example.org:8443/a#b",
		"echofog://contact?v=2&card=AAAA",
		"https://" + strings.Repeat("a", 192),
	}
	bad := []string{
		"",
		"http://shop.example.org/",
		"HTTPS://shop.example.org/",
		"javascript:alert(1)",
		"tg://resolve?domain=x",
		"ftp://example.org/",
		"echofog://join?v=2&card=AAAA",
		"echofog://contact?",
		"https://t.me@evil.example/",
		"https://user:pw@example.org/",
		"https:///nohost",
		"https://exa mple.org/",
		"https://example.org/\n",
		"https://example.org\\@evil/",
		"https://" + strings.Repeat("a", 193),
		"data:text/html,hi",
	}
	for _, s := range ok {
		if err := ValidateShopLink(s); err != nil {
			t.Errorf("%q rejected: %v", s, err)
		}
	}
	for _, s := range bad {
		if err := ValidateShopLink(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q accepted: %v", s, err)
		}
	}
	// An invalid link anywhere in the list invalidates the card, even beyond the first 3.
	f := newFixture(t)
	c := f.public(t)
	c.Shop = []string{"https://a.example/", "https://b.example/", "https://c.example/", "http://d.example/"}
	_, err := VerifyCard(f.seal(t, c), vectorNow)
	mustInvalid(t, err, "fourth shop link http://")
}

// ---------------------------------------------------------------------------------------------
// Release manifests (D16, versioning.md §5)

type releaseKeys struct {
	k [6]*keys.Signer
}

func newReleaseKeys(t testing.TB) releaseKeys {
	var r releaseKeys
	for i := range r.k {
		r.k[i] = testSigner(t, "release-key-"+string(rune('a'+i)))
	}
	return r
}

func (r releaseKeys) compiled() []keys.AKey {
	return []keys.AKey{r.k[0].Public(), r.k[1].Public(), r.k[2].Public()}
}

func rel(at uint64, mut func(*pb.Release)) *pb.Release {
	m := &pb.Release{Version: "0.3.0", ReleasedAt: at,
		Policies: []*pb.Policy{{Component: "proto", MinSupported: 200, Recommended: 200}}}
	if mut != nil {
		mut(m)
	}
	return m
}

func sealRel(t testing.TB, signer *keys.Signer, m *pb.Release, co ...*keys.Signer) []byte {
	t.Helper()
	b, err := SealRelease(signer, m.GetReleasedAt()+30*day, m, co...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReleaseThreshold(t *testing.T) {
	r := newReleaseKeys(t)
	st, err := NewReleaseState(r.compiled())
	if err != nil {
		t.Fatal(err)
	}
	m := rel(fixedTime, nil)
	// One signature is not enough; the same key twice is one key; keys outside the set do not count.
	for name, env := range map[string][]byte{
		"envelope only":        sealRel(t, r.k[0], m),
		"same key twice":       sealRel(t, r.k[0], m, r.k[0]),
		"cosigner not in set":  sealRel(t, r.k[0], m, r.k[4]),
		"signer not in set":    sealRel(t, r.k[4], m, r.k[0]),
		"both outside the set": sealRel(t, r.k[4], m, r.k[5]),
		"cosig over another manifest": func() []byte {
			other, _ := Marshal(rel(fixedTime+1, nil))
			own, _ := Marshal(m)
			cs := Cosign(r.k[1], other)
			body, _ := Marshal(&pb.ReleaseObject{Manifest: own, Cosigs: []*pb.CoSig{cs}})
			b, _ := Seal(LabelRelease, r.k[0], fixedTime, fixedTime+day, body)
			return b
		}(),
	} {
		if _, err := st.Check(env, vectorNow); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	got, err := st.Check(sealRel(t, r.k[2], m, r.k[4], r.k[1]), vectorNow)
	if err != nil || len(got.Signers) != 2 {
		t.Fatalf("2 of 3 with an extra foreign cosig: %v", err)
	}

	// seq must equal released_at.
	body, _ := Marshal(&pb.ReleaseObject{Manifest: mustMarshal(t, m), Cosigs: []*pb.CoSig{Cosign(r.k[1], mustMarshal(t, m))}})
	wrongSeq, _ := Seal(LabelRelease, r.k[0], fixedTime+5, fixedTime+day, body)
	if _, err := st.Check(wrongSeq, vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("seq ≠ released_at: %v", err)
	}
	// released_at at most one day in the future.
	if _, err := st.Check(sealRel(t, r.k[0], rel(fixedTime+day, nil), r.k[1]), vectorNow); err != nil {
		t.Errorf("released_at = now + 1 day: %v", err)
	}
	if _, err := st.Check(sealRel(t, r.k[0], rel(fixedTime+day+1, nil), r.k[1]), vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("released_at = now + 1 day + 1 s: %v", err)
	}
	// https only.
	for _, u := range []string{"http://example.org/notes", "ftp://example.org/"} {
		if _, err := st.Check(sealRel(t, r.k[0], rel(fixedTime, func(m *pb.Release) { m.NotesUrl = u }), r.k[1]), vectorNow); !errors.Is(err, ErrInvalid) {
			t.Errorf("notes_url %q: %v", u, err)
		}
		bad := rel(fixedTime, func(m *pb.Release) { m.Artifacts = []*pb.Artifact{{Platform: "linux-amd64", Url: u}} })
		if _, err := st.Check(sealRel(t, r.k[0], bad, r.k[1]), vectorNow); !errors.Is(err, ErrInvalid) {
			t.Errorf("artifact url %q: %v", u, err)
		}
	}

	// Apply, then the next one must be newer.
	if _, err := st.Apply(sealRel(t, r.k[0], m, r.k[1]), vectorNow); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Check(sealRel(t, r.k[0], m, r.k[2]), vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("same released_at again: %v", err)
	}
	if _, err := st.Check(sealRel(t, r.k[0], rel(fixedTime-1, nil), r.k[2]), vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("older released_at: %v", err)
	}
	if _, err := st.Check(sealRel(t, r.k[0], rel(fixedTime+1, nil), r.k[2]), vectorNow); err != nil {
		t.Errorf("newer manifest: %v", err)
	}
	// A huge seq cannot block later manifests: it is refused (seq = released_at ≤ now + 1 day).
	huge, err := SealRelease(r.k[0], fixedTime+30*day, rel(1<<60, nil), r.k[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Check(huge, vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("huge seq: %v", err)
	}
	// Expired manifests are not applied.
	if _, err := st.Check(sealRel(t, r.k[0], rel(fixedTime+2, nil), r.k[1]), time.Unix(int64(fixedTime+40*day), 0)); !errors.Is(err, ErrExpired) {
		t.Errorf("expired manifest: %v", err)
	}
}

func mustMarshal(t testing.TB, m proto.Message) []byte {
	t.Helper()
	b, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReleaseKeyRotation(t *testing.T) {
	r := newReleaseKeys(t)
	st, _ := NewReleaseState(r.compiled())
	a, b, c, d := r.k[0], r.k[1], r.k[2], r.k[3]

	// Revoke c and add d, signed by a and b.
	m1 := rel(fixedTime, func(m *pb.Release) {
		m.NextKeys = [][]byte{d.Public()}
		m.RevokedKeys = [][]byte{c.Public()}
	})
	if _, err := st.Apply(sealRel(t, a, m1, b), vectorNow); err != nil {
		t.Fatal(err)
	}
	set := st.Keys()
	if len(set) != 3 || containsKey(set, c.Public()) || !containsKey(set, d.Public()) {
		t.Fatalf("key set after rotation: %d keys", len(set))
	}
	// The revoked key no longer counts; the new one does.
	if _, err := st.Check(sealRel(t, c, rel(fixedTime+1, nil), a), vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("revoked key signed: %v", err)
	}
	if _, err := st.Check(sealRel(t, d, rel(fixedTime+1, nil), a), vectorNow); err != nil {
		t.Errorf("new key: %v", err)
	}

	// Signers may not revoke themselves.
	st2, _ := NewReleaseState(r.compiled())
	self := rel(fixedTime, func(m *pb.Release) { m.RevokedKeys = [][]byte{a.Public()}; m.NextKeys = [][]byte{d.Public()} })
	if _, err := st2.Check(sealRel(t, a, self, b), vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("signer revokes itself: %v", err)
	}
	// The set never has fewer than 2 or more than 5 keys.
	tooFew := rel(fixedTime, func(m *pb.Release) { m.RevokedKeys = [][]byte{c.Public()} })
	if _, err := st2.Check(sealRel(t, a, tooFew, b), vectorNow); err != nil {
		t.Errorf("3 → 2 keys allowed: %v", err)
	}
	st3, _ := NewReleaseState([]keys.AKey{a.Public(), b.Public()})
	if _, err := st3.Check(sealRel(t, a, rel(fixedTime, func(m *pb.Release) { m.RevokedKeys = [][]byte{c.Public(), b.Public()} }), b), vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("signer b revoked: %v", err)
	}
	e, f := r.k[4], r.k[5]
	tooMany := rel(fixedTime, func(m *pb.Release) { m.NextKeys = [][]byte{d.Public(), e.Public(), f.Public()} })
	if _, err := st2.Check(sealRel(t, a, tooMany, b), vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("6 keys: %v", err)
	}
	// next_keys is processed up to 3 entries.
	four := rel(fixedTime, func(m *pb.Release) {
		m.NextKeys = [][]byte{d.Public(), e.Public(), f.Public(), testSigner(t, "x").Public()}
		m.RevokedKeys = [][]byte{c.Public(), d.Public()}
	})
	got, err := st2.Check(sealRel(t, a, four, b), vectorNow)
	if err != nil || len(got.Keys) != 4 {
		t.Errorf("4 next keys, first 3 used: %v", err)
	}
	if _, err := NewReleaseState([]keys.AKey{a.Public()}); err == nil {
		t.Error("one compiled key accepted")
	}
}

func TestReleaseNeverLockOut(t *testing.T) {
	r := newReleaseKeys(t)
	at := fixedTime
	apply := func(st *ReleaseState, m *pb.Release) {
		t.Helper()
		if _, err := st.Apply(sealRel(t, r.k[0], m, r.k[1]), time.Unix(int64(m.GetReleasedAt()), 0)); err != nil {
			t.Fatal(err)
		}
	}
	pol := func(p ...*pb.Policy) func(*pb.Release) { return func(m *pb.Release) { m.Policies = p } }
	now := time.Unix(int64(at), 0)

	// min_supported above the node's own version: shown as update required, never a refusal.
	st, _ := NewReleaseState(r.compiled())
	apply(st, rel(at, pol(&pb.Policy{Component: "svc:3", MinSupported: 3, Recommended: 3})))
	if st.PeerRefused("svc:3", 2, 2, now) || st.PeerRefused("svc:3", 2, 1, now) {
		t.Error("entry covering the node's own version caused a refusal")
	}
	if st.Status("svc:3", 2, now) != UpdateRequired {
		t.Error("update required not reported")
	}

	// Refusals only for peers strictly below the node's own version.
	st, _ = NewReleaseState(r.compiled())
	apply(st, rel(at, pol(&pb.Policy{Component: "svc:3", MinSupported: 3, Recommended: 4})))
	if !st.PeerRefused("svc:3", 4, 2, now) || st.PeerRefused("svc:3", 4, 3, now) || st.PeerRefused("svc:3", 4, 5, now) {
		t.Error("svc:3 refusals")
	}
	if st.Status("svc:3", 3, now) != UpdateRecommended || st.Status("svc:3", 4, now) != UpToDate {
		t.Error("svc:3 status")
	}
	if st.PeerRefused("svc:9", 2, 1, now) {
		t.Error("component without a policy refused")
	}

	// min_supported may rise by at most one major version per manifest.
	st, _ = NewReleaseState(r.compiled())
	apply(st, rel(at, pol(&pb.Policy{Component: "proto", MinSupported: 400, Recommended: 400})))
	if p := st.Policy("proto"); p.GetMinSupported() != 300 {
		t.Errorf("proto min_supported raised to %d", p.GetMinSupported())
	}
	if !st.PeerRefused("proto", 300, 299, now) || st.PeerRefused("proto", 300, 300, now) || st.PeerRefused("proto", 299, 200, now) {
		t.Error("proto refusals after a clipped raise")
	}
	apply(st, rel(at+1, pol(&pb.Policy{Component: "svc:3", MinSupported: 2}, &pb.Policy{Component: "proto", MinSupported: 500})))
	if p := st.Policy("proto"); p.GetMinSupported() != 400 {
		t.Errorf("second raise to %d", p.GetMinSupported())
	}
	apply(st, rel(at+2, pol(&pb.Policy{Component: "svc:3", MinSupported: 9})))
	if p := st.Policy("svc:3"); p.GetMinSupported() != 3 {
		t.Errorf("svc:3 raised to %d", p.GetMinSupported())
	}

	// deprecated_after: ignored when sooner than 90 days after released_at; afterwards versions
	// below recommended count as below min_supported (never the node's own).
	st, _ = NewReleaseState(r.compiled())
	apply(st, rel(at, pol(
		&pb.Policy{Component: "l1:1", MinSupported: 2, Recommended: 3, DeprecatedAfter: at + 89*day},
		&pb.Policy{Component: "l0:1", MinSupported: 2, Recommended: 3, DeprecatedAfter: at + 90*day},
	)))
	later := time.Unix(int64(at+100*day), 0)
	if st.PeerRefused("l1:1", 3, 2, later) {
		t.Error("instant deprecation applied")
	}
	if !st.PeerRefused("l0:1", 3, 2, later) || st.PeerRefused("l0:1", 3, 2, now) {
		t.Error("deprecation after 90 days")
	}
	if st.PeerRefused("l0:1", 2, 2, later) || st.PeerRefused("l0:1", 2, 1, later) {
		t.Error("deprecation locked out the node's own version")
	}
	if st.Status("l0:1", 2, later) != UpdateRequired {
		t.Error("deprecated own version not reported")
	}
}

// ---------------------------------------------------------------------------------------------
// Prekeys, mailbox hints, name bindings

func TestPrekeys(t *testing.T) {
	f := newFixture(t)
	good := &pb.Prekeys{Ik: keys.New(keys.X25519, f.cv.mask3), Spk: make([]byte, 32), SpkCreated: fixedTime,
		Kem: make([]byte, 1184), KemCreated: fixedTime, Opks: [][]byte{make([]byte, 32)}}
	seal := func(p *pb.Prekeys, life uint64) []byte {
		b, err := Seal(LabelPrekeys, f.device, fixedTime, fixedTime+life, mustMarshal(t, p))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	pk, err := VerifyPrekeys(seal(good, 30*day), vectorNow)
	if err != nil {
		t.Fatal(err)
	}
	dl, _ := VerifyDeviceList(f.devEnv, vectorNow)
	if !pk.VouchedBy(dl) || pk.KEMKeyID() != [32]byte(sha(good.Kem)) || len(pk.OPKs()) != 1 {
		t.Fatal("prekeys accessors")
	}
	for name, mut := range map[string]func(p *pb.Prekeys){
		"no kem (v1 downgrade)": func(p *pb.Prekeys) { p.Kem = nil },
		"short kem":             func(p *pb.Prekeys) { p.Kem = make([]byte, 1183) },
		"ik not akey":           func(p *pb.Prekeys) { p.Ik = make([]byte, 32) },
		"spk length":            func(p *pb.Prekeys) { p.Spk = make([]byte, 33) },
		"opk length":            func(p *pb.Prekeys) { p.Opks = [][]byte{make([]byte, 31)} },
	} {
		p := proto.Clone(good).(*pb.Prekeys)
		mut(p)
		_, err := VerifyPrekeys(seal(p, day), vectorNow)
		mustInvalid(t, err, name)
	}
	if _, err := VerifyPrekeys(seal(good, 31*day), vectorNow); !errors.Is(err, ErrLifetime) {
		t.Fatalf("31-day prekeys: %v", err)
	}
}

func TestMailboxHintAndName(t *testing.T) {
	f := newFixture(t)
	relayCard := f.cv.inviteCard(t)
	cardEnv := sealBody(t, f.device, fixedTime, fixedTime+day, relayCard)
	did := f.device.Public().ID()
	h := &pb.MailboxHint{Hints: []*pb.Hint{{Relay: did[:], Queue: make([]byte, 16), NotAfter: fixedTime + hour}}}
	env, err := Seal(LabelMailboxHint, f.device, 1, fixedTime+6*hour, mustMarshal(t, h))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyMailboxHint(env, vectorNow); err != nil {
		t.Fatal(err)
	}
	h.Hints[0].RelayCard = cardEnv
	env, err = Seal(LabelMailboxHint, f.device, 1, fixedTime+6*hour, mustMarshal(t, h))
	if err != nil {
		t.Fatal(err)
	}
	mh, err := VerifyMailboxHint(env, vectorNow)
	if err != nil || mh.RelayCards[0] == nil {
		t.Fatalf("hint with relay card: %v", err)
	}
	// At most one relay card per object.
	two := proto.Clone(h).(*pb.MailboxHint)
	two.Hints = append(two.Hints, proto.Clone(h.Hints[0]).(*pb.Hint))
	env, err = Seal(LabelMailboxHint, f.device, 1, fixedTime+6*hour, mustMarshal(t, two))
	if err != nil {
		t.Fatal(err)
	}
	_, err = VerifyMailboxHint(env, vectorNow)
	mustInvalid(t, err, "two relay cards")
	// The body limit is 6144 bytes.
	if _, err := Verify(sealRaw(LabelMailboxHint, f.device, 1, fixedTime+hour, appendField(nil, 1, protowire.BytesType, make([]byte, 6200))), vectorNow); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized mailbox hint: %v", err)
	}
	h.Hints[0].Relay = make([]byte, 32)
	h.Hints[0].RelayCard = nil
	h.Hints[0].Queue = make([]byte, 15)
	env, _ = Seal(LabelMailboxHint, f.device, 1, fixedTime+6*hour, mustMarshal(t, h))
	_, err = VerifyMailboxHint(env, vectorNow)
	mustInvalid(t, err, "queue of 15 bytes")

	nb := &pb.NameBinding{Name: "alice.ton", System: NameSystemTON, Owner: f.account.Public(), Issued: fixedTime}
	env, _ = Seal(LabelName, f.account, 1, fixedTime+day, mustMarshal(t, nb))
	if _, err := VerifyNameBinding(env, vectorNow); err != nil {
		t.Fatal(err)
	}
	nb.Name = ""
	env, _ = Seal(LabelName, f.account, 1, fixedTime+day, mustMarshal(t, nb))
	_, err = VerifyNameBinding(env, vectorNow)
	mustInvalid(t, err, "empty name")
}

// ---------------------------------------------------------------------------------------------
// Links

func TestLinkRules(t *testing.T) {
	cv := loadCardVector(t)
	lv := loadVector(t, "link.json")
	contact := str(t, lv, "contact", "link")
	invite := str(t, lv, "invitation", "link")
	card64 := strings.TrimPrefix(contact, "echofog://join?v=2&card=")

	if _, err := ParseLink("echofog://join?v=3&card=" + card64); !errors.Is(err, ErrOtherVersion) {
		t.Errorf("v=3: %v", err)
	}
	if l, err := ParseLink("echofog://join?x=1&v=2&y=2&card=" + card64 + "&z"); err != nil || l.Card == nil {
		t.Errorf("unknown parameters not ignored: %v", err)
	}
	for name, s := range map[string]string{
		"card before v":      "echofog://join?card=" + card64 + "&v=2",
		"s before card":      "echofog://join?v=2&s=f09YO-P_Q8eukGYY4gQQsg&card=" + card64,
		"v twice":            "echofog://join?v=2&v=2&card=" + card64,
		"no card":            "echofog://join?v=2",
		"padded base64":      "echofog://join?v=2&card=" + card64 + "=",
		"std alphabet":       "echofog://join?v=2&card=" + strings.NewReplacer("-", "+", "_", "/").Replace(card64),
		"unknown method":     "echofog://join?v=2&card=Ag" + card64[2:],
		"truncated deflate":  "echofog://join?v=2&card=" + card64[:len(card64)/2],
		"short secret":       contact + "&s=AAAA",
		"other scheme":       "echofog://contact?v=2&card=" + card64,
		"web without marker": "https://example.org/x/#join?v=2&card=" + card64,
	} {
		if _, err := ParseLink(s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// DEFLATE bomb: stops after 4097 output bytes.
	bomb := deflate(t, bytes.Repeat([]byte{0}, 1<<20))
	if _, err := ParseLink("echofog://join?v=2&card=" + b64.EncodeToString(append([]byte{1}, bomb...))); !errors.Is(err, ErrBadLink) {
		t.Errorf("deflate bomb: %v", err)
	}
	// Raw card of 4097 bytes is too large.
	if _, err := ParseLink("echofog://join?v=2&card=" + b64.EncodeToString(append([]byte{0}, make([]byte, 4097)...))); !errors.Is(err, ErrBadLink) {
		t.Errorf("4097-byte raw card: %v", err)
	}
	// Raw method decodes too.
	pub := hx(t, cv.v, "public", "envelope")
	l, err := ParseLink("echofog://join?v=2&card=" + b64.EncodeToString(append([]byte{0}, pub...)))
	if err != nil || !bytes.Equal(l.Card, pub) || l.Method != 0 {
		t.Fatalf("raw method: %v", err)
	}

	// Scope and secret must match.
	li, _ := ParseLink(invite)
	li.Secret = make([]byte, 16)
	if _, err := li.Verify(vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("wrong secret: %v", err)
	}
	li, _ = ParseLink(invite)
	li.Secret = nil
	if _, err := li.Verify(vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("invite card in a contact link: %v", err)
	}
	peer, _ := EncodeLink(hx(t, cv.v, "peer", "envelope"), nil)
	lp, _ := ParseLink(peer)
	if _, err := lp.Verify(vectorNow); !errors.Is(err, ErrInvalid) {
		t.Errorf("peer card in a contact link: %v", err)
	}
	if _, err := EncodeLink(make([]byte, 4097), nil); err == nil {
		t.Error("encoded a 4097-byte card")
	}
	// Tie or raw shorter: method 0.
	tiny := []byte{1, 2, 3}
	s, _ := EncodeLink(tiny, nil)
	if l, _ := ParseLink(s); l.Method != 0 {
		t.Error("short input not raw")
	}
}

// ---------------------------------------------------------------------------------------------
// Card book (objects.md §3.2)

func TestCardBook(t *testing.T) {
	f := newFixture(t)
	book := NewCardBook()
	mk := func(seq uint64, mut func(*pb.NodeCard)) *Card {
		c := f.public(t)
		if mut != nil {
			mut(c)
		}
		card, err := VerifyCard(sealBody(t, f.device, seq, fixedTime+6*day, c), vectorNow)
		if err != nil {
			t.Fatal(err)
		}
		return card
	}
	c1 := mk(fixedTime, nil)
	if r, err := book.Add(c1); r != CardAdded || err != nil {
		t.Fatalf("add: %v %v", r, err)
	}
	if r, _ := book.Add(c1); r != CardIgnored {
		t.Fatal("duplicate not ignored")
	}
	c2 := mk(fixedTime+5, func(c *pb.NodeCard) { c.Release = "0.3.1" })
	if r, _ := book.Add(c2); r != CardReplaced {
		t.Fatal("newer card not taken")
	}
	if r, _ := book.Add(c1); r != CardIgnored || book.Get(c1.DeviceID, ScopePublic, nil) != c2 {
		t.Fatal("older card replaced the newer one")
	}
	// Equivocation: same seq, other bytes; the smaller object_id wins, the signer is flagged.
	c3 := mk(fixedTime+5, func(c *pb.NodeCard) { c.Release = "0.3.2" })
	_, err := book.Add(c3)
	if !errors.Is(err, ErrEquivocation) || !book.Misbehaving(c1.DeviceID) {
		t.Fatalf("equivocation: %v", err)
	}
	want := Prefer(c2.Env, c3.Env)
	if book.Get(c1.DeviceID, ScopePublic, nil).Env != want {
		t.Fatal("equivocation kept the wrong card")
	}
	// Scopes are separate slots; Latest is the highest seq.
	pc := f.cv.peerCard(t)
	peer, err := VerifyCard(sealBody(t, f.device, fixedTime+9, fixedTime+6*day, pc), vectorNow)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := book.Add(peer); r != CardAdded || err != nil {
		t.Fatalf("peer card: %v %v", r, err)
	}
	if book.Latest(c1.DeviceID) != peer {
		t.Fatal("Latest")
	}

	// A card alone does not bind its handshake key: the binding comes from a completed handshake
	// (objects.md §3.2, handshake.md §7). Another device's card that claims this hs key is stored
	// separately and must not shadow the device that owns it.
	other := testSigner(t, "book-other-device")
	list, _ := SealDeviceList(f.account, fixedTime+1, fixedTime+30*day, &pb.DeviceList{Devices: []*pb.Device{
		{Key: f.device.Public(), Added: fixedTime, Flags: DevicePrimary}, {Key: other.Public(), Added: fixedTime}}})
	oc := f.public(t)
	oc.Devices = list
	stolen, err := VerifyCard(sealBody(t, other, fixedTime, fixedTime+6*day, oc), vectorNow)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := book.Add(stolen); err != nil {
		t.Fatalf("a card alone must not bind hs_key: %v", err)
	} else if r != CardAdded {
		t.Fatalf("card of another device: result %v, want added", r)
	}
	if book.Get(other.Public().ID(), ScopePublic, nil) == nil {
		t.Fatal("the other device's card was not stored")
	}
	if book.Get(c1.DeviceID, ScopePublic, nil) == nil {
		t.Fatal("the device that owns the hs key lost its card")
	}

	// A newer device list without the device removes it.
	list2, _ := SealDeviceList(f.account, fixedTime+2, fixedTime+30*day, &pb.DeviceList{Devices: []*pb.Device{
		{Key: other.Public(), Added: fixedTime, Flags: DevicePrimary}}})
	dl2, err := VerifyDeviceList(list2, vectorNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := book.AddDeviceList(dl2); err != nil {
		t.Fatal(err)
	}
	if book.Latest(c1.DeviceID) != nil || book.Get(c1.DeviceID, ScopePublic, nil) != nil {
		t.Fatal("card of a removed device still served")
	}
	if _, err := book.Add(mk(fixedTime+20, nil)); !errors.Is(err, ErrDeviceRemoved) {
		t.Fatalf("card with an older device list: %v", err)
	}
	if book.DeviceList(c1.NodeID) != dl2 {
		t.Fatal("newest device list not kept")
	}
}
