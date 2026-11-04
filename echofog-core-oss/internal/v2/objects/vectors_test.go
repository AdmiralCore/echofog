package objects

import (
	"bytes"
	"crypto/ecdh"
	"errors"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/keys"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// ---------------------------------------------------------------------------------------------
// varint.json: the envelope's body_len is a QUIC varint.

func TestVectorVarint(t *testing.T) {
	v := loadVector(t, "varint.json")
	for _, e := range get(t, v, "valid").([]any) {
		o := e.(map[string]any)
		val := uint64(o["value"].(float64))
		enc := unhex(t, o["encoded"].(string))
		// float64 loses precision on the largest values; recompute them from the encoding.
		if val > 1<<52 {
			got, n, err := readVarint(enc)
			if err != nil || n != len(enc) {
				t.Fatalf("decode %x: %v", enc, err)
			}
			val = got
		}
		eqHex(t, "varint encode", appendVarint(nil, val), enc)
		got, n, err := readVarint(enc)
		if err != nil || got != val || n != len(enc) {
			t.Fatalf("decode %x = %d,%d,%v want %d", enc, got, n, err, val)
		}
	}
	for _, e := range get(t, v, "invalid").([]any) {
		enc := unhex(t, e.(map[string]any)["encoded"].(string))
		if _, _, err := readVarint(enc); err == nil {
			t.Fatalf("non-minimal varint %x accepted", enc)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// signed-object.json

func TestVectorSignedObjectKeys(t *testing.T) {
	v := loadVector(t, "signed-object.json")
	s := signer(t, str(t, v, "signer_seed"))
	eqHex(t, "signer_akey", s.Public(), hx(t, v, "signer_akey"))
	if got := s.Public().Text(); got != str(t, v, "signer_text") {
		t.Fatalf("text form %q", got)
	}
	k, err := keys.ParseText(str(t, v, "signer_text"))
	if err != nil || !k.Equal(s.Public()) {
		t.Fatalf("ParseText: %v", err)
	}
	id := s.Public().ID()
	eqHex(t, "node_id", id[:], hx(t, v, "node_id"))
}

func vectorManifest() *pb.Release {
	return &pb.Release{
		Version:    "0.3.0",
		ReleasedAt: fixedTime,
		Policies:   []*pb.Policy{{Component: "proto", MinSupported: 200, Recommended: 200}},
		Artifacts: []*pb.Artifact{{
			Platform: "windows-amd64",
			Name:     "EchoFog-0.3.0-windows-amd64.zip",
			Sha256:   sha([]byte("artifact bytes")),
			Size:     52428800,
			Url:      "https://example.org/releases/0.3.0/EchoFog-0.3.0-windows-amd64.zip",
		}},
		NotesUrl: "https://example.org/releases/0.3.0",
	}
}

func TestVectorSignedObjectRelease(t *testing.T) {
	v := loadVector(t, "signed-object.json")
	o := get(t, v, "objects").([]any)[0].(map[string]any)
	s := signer(t, str(t, v, "signer_seed"))
	s2 := signer(t, str(t, v, "seeds", "release/second_key"))

	manifest, err := Marshal(vectorManifest())
	if err != nil {
		t.Fatal(err)
	}
	eqHex(t, "manifest", manifest, hx(t, o, "manifest"))
	eqHex(t, "cosigner_akey", s2.Public(), hx(t, o, "cosigner_akey"))
	eqHex(t, "cosig_input", CosignInput(manifest), hx(t, o, "cosig_input"))
	cs := Cosign(s2, manifest)
	eqHex(t, "cosig", cs.Sig, hx(t, o, "cosig"))

	body, err := Marshal(&pb.ReleaseObject{Manifest: manifest, Cosigs: []*pb.CoSig{cs}})
	if err != nil {
		t.Fatal(err)
	}
	eqHex(t, "body", body, hx(t, o, "body"))

	env, err := Seal(str(t, o, "label"), s, num(t, o, "seq"), num(t, o, "not_after"), body)
	if err != nil {
		t.Fatal(err)
	}
	eqHex(t, "envelope", env, hx(t, o, "envelope"))

	e, err := Verify(env, vectorNow)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	eqHex(t, "signed_bytes", e.SignedBytes(), hx(t, o, "signed_bytes"))
	eqHex(t, "sig", e.Sig, hx(t, o, "sig"))
	id := e.ID()
	eqHex(t, "object_id", id[:], hx(t, o, "object_id"))
	if e.Label != LabelRelease || e.Seq != fixedTime || !e.Signer.Equal(s.Public()) {
		t.Fatalf("parsed envelope fields: %+v", e)
	}

	// Threshold verification with a compiled key set holding both keys and a third one.
	rs, err := NewReleaseState([]keys.AKey{s.Public(), s2.Public(), testSigner(t, "release/third").Public()})
	if err != nil {
		t.Fatal(err)
	}
	m, err := rs.Apply(env, vectorNow)
	if err != nil {
		t.Fatalf("release Apply: %v", err)
	}
	if m.Release.GetVersion() != "0.3.0" || len(m.Signers) != 2 {
		t.Fatalf("manifest: %v signers %d", m.Release, len(m.Signers))
	}
}

func TestVectorSignedObjectEmptyBody(t *testing.T) {
	v := loadVector(t, "signed-object.json")
	o := get(t, v, "objects").([]any)[1].(map[string]any)
	s := signer(t, str(t, v, "signer_seed"))
	env, err := Seal(str(t, o, "label"), s, num(t, o, "seq"), num(t, o, "not_after"), nil)
	if err != nil {
		t.Fatal(err)
	}
	eqHex(t, "envelope", env, hx(t, o, "envelope"))
	e, err := Verify(env, vectorNow)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(e.Body) != 0 || e.Seq != 1 {
		t.Fatalf("envelope %+v", e)
	}
	dl, err := VerifyDeviceList(env, vectorNow)
	if err != nil {
		t.Fatalf("VerifyDeviceList: %v", err)
	}
	if len(dl.Devices()) != 0 {
		t.Fatal("empty list has devices")
	}
}

// ---------------------------------------------------------------------------------------------
// node-card.json

type cardVector struct {
	v                     map[string]any
	account, device       *keys.Signer
	hsAkey, prevHS        []byte
	maskR, mask3, maskInv []byte
	devEnv                []byte
}

func loadCardVector(t testing.TB) cardVector {
	v := loadVector(t, "node-card.json")
	return cardVector{
		v:       v,
		account: signer(t, str(t, v, "account_seed")),
		device:  signer(t, str(t, v, "device_seed")),
		hsAkey:  hx(t, v, "hs_akey"),
		prevHS:  hx(t, v, "prev_hs_akey"),
		maskR:   hx(t, v, "mask_keys", "reality (reality2.json)"),
		mask3:   hx(t, v, "mask_keys", "noise3 (noise3.json)"),
		maskInv: hx(t, v, "mask_keys", "invitation"),
		devEnv:  hx(t, v, "device_list", "envelope"),
	}
}

func shape(id uint32) *pb.ShapeProfile { return &pb.ShapeProfile{Id: id} }

func params(t testing.TB, sni string, shapeID uint32) []byte {
	b, err := Marshal(&pb.SuiteParams{Sni: sni, Shape: shape(shapeID)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// vectorCard builds the card body of the generator's build() function.
func (cv cardVector) card(t testing.TB, addrs []*pb.Addr, masks [][]byte, scope uint32, inviteID []byte, flags uint32, withPrev bool) *pb.NodeCard {
	notAfter := fixedTime + 6*86400
	c := &pb.NodeCard{
		ProtoVer: 200,
		HsKey:    cv.hsAkey,
		L1Suites: []uint32{1, 2},
		Addrs:    addrs,
		Role:     1,
		Flags:    flags,
		Features: []uint32{1, 3, 4, 5, 6},
		Exit: &pb.ExitOffer{Price: 500, FreeKib: 10240, UploadKbps: 100000, Region: "NL", Policy: 0b1011,
			Assets: []uint32{1, 2}},
		Shop:     []string{"https://shop.example.org/echofog"},
		Release:  "0.3.0",
		Account:  cv.account.Public(),
		Scope:    scope,
		InviteId: inviteID,
		Devices:  cv.devEnv,
	}
	for _, m := range masks {
		c.MaskKeys = append(c.MaskKeys, &pb.MaskKey{Key: keys.New(keys.X25519, m), NotAfter: notAfter + 86400})
	}
	for _, id := range []uint32{1, 2, 5, 6} {
		c.Services = append(c.Services, &pb.ServiceOffer{Id: id, Versions: []uint32{2}})
	}
	if withPrev {
		c.PrevHs = &pb.HsKey{Key: cv.prevHS, NotAfter: notAfter}
	}
	return c
}

func (cv cardVector) publicCard(t testing.TB) *pb.NodeCard {
	return cv.card(t, []*pb.Addr{
		{Suite: 2, Host: "192.0.2.10", Port: 443, Mask: 1, Params: params(t, "www.example.org", 1), Flags: 1},
		{Suite: 3, Host: "198.51.100.7", Port: 443, Mask: 1, Params: params(t, "www.example.org", 2)},
	}, [][]byte{cv.maskR}, 0, nil, 0b100110, false)
}

func (cv cardVector) peerCard(t testing.TB) *pb.NodeCard {
	return cv.card(t, []*pb.Addr{
		{Suite: 1, Host: "192.0.2.10", Port: 443, Mask: 2, Params: params(t, "", 1)},
		{Suite: 2, Host: "192.0.2.10", Port: 443, Mask: 1, Params: params(t, "www.example.org", 1), Flags: 1},
		{Suite: 3, Host: "198.51.100.7", Port: 443, Mask: 1, Params: params(t, "www.example.org", 2)},
		{Suite: 4, Url: "wss://cdn.example.org/ef/3c9a7d", Mask: 1, Params: params(t, "inner.example.org", 2)},
	}, [][]byte{cv.maskR, cv.mask3}, 1, nil, 0b100110, true)
}

// inviteCard lists only suite 1 (a hidden node never lists XX) and has no exit offer.
func (cv cardVector) inviteCard(t testing.TB) *pb.NodeCard {
	c := cv.card(t, []*pb.Addr{{Suite: 1, Host: "192.0.2.10", Port: 443, Mask: 1, Params: params(t, "", 1)}},
		[][]byte{cv.maskInv}, 2, hx(t, cv.v, "invite", "invite_id"), 0b010001, false)
	c.L1Suites = []uint32{1}
	c.Exit = nil
	return c
}

func TestVectorDeviceList(t *testing.T) {
	cv := loadCardVector(t)
	v := cv.v
	eqHex(t, "account_pub", cv.account.Public().Key(), hx(t, v, "account_pub"))
	eqHex(t, "device_pub", cv.device.Public().Key(), hx(t, v, "device_pub"))
	nid, did := cv.account.Public().ID(), cv.device.Public().ID()
	eqHex(t, "node_id", nid[:], hx(t, v, "node_id (SHA-256 of the account akey)"))
	eqHex(t, "device_id", did[:], hx(t, v, "device_id (SHA-256 of the device akey; the card signer)"))
	if cv.account.Public().Text() != str(t, v, "account_text") {
		t.Fatal("account text form")
	}

	body, err := Marshal(&pb.DeviceList{Devices: []*pb.Device{{Key: cv.device.Public(), Added: fixedTime, Flags: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	eqHex(t, "device list body", body, hx(t, v, "device_list", "body"))
	env, err := Seal(LabelDevices, cv.account, fixedTime, fixedTime+30*86400, body)
	if err != nil {
		t.Fatal(err)
	}
	eqHex(t, "device list envelope", env, cv.devEnv)
	dl, err := VerifyDeviceList(env, vectorNow)
	if err != nil {
		t.Fatalf("VerifyDeviceList: %v", err)
	}
	if !dl.Contains(cv.device.Public()) || dl.NodeID != nid {
		t.Fatal("device list does not list the device")
	}
	if p := dl.Devices()[0]; p.GetFlags()&DevicePrimary == 0 {
		t.Fatal("primary flag")
	}
}

func TestVectorCards(t *testing.T) {
	cv := loadCardVector(t)
	v := cv.v
	nid := cv.account.Public().ID()
	did := cv.device.Public().ID()

	cases := []struct {
		name     string
		body     *pb.NodeCard
		notAfter uint64
		scope    Scope
	}{
		{"public", cv.publicCard(t), fixedTime + 6*86400, ScopePublic},
		{"peer", cv.peerCard(t), fixedTime + 6*86400, ScopePeer},
		{"invite", cv.inviteCard(t), fixedTime + 30*86400, ScopeInvite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := get(t, v, c.name).(map[string]any)
			if c.name == "public" {
				body, err := Marshal(c.body)
				if err != nil {
					t.Fatal(err)
				}
				eqHex(t, "public body", body, hx(t, o, "body"))
			}
			env, err := SealCard(cv.device, num(t, o, "seq"), c.notAfter, c.body)
			if err != nil {
				t.Fatalf("SealCard: %v", err)
			}
			eqHex(t, "envelope", env, hx(t, o, "envelope"))
			if uint64(len(env)) != num(t, o, "envelope_len") {
				t.Fatalf("envelope_len %d", len(env))
			}
			card, err := VerifyCard(env, vectorNow)
			if err != nil {
				t.Fatalf("VerifyCard: %v", err)
			}
			if card.NodeID != nid || card.DeviceID != did || card.Scope != c.scope {
				t.Fatalf("card ids/scope: %x %x %v", card.NodeID, card.DeviceID, card.Scope)
			}
			if !bytes.Equal(card.Env.Raw, env) {
				t.Fatal("card does not keep the received bytes")
			}
			if c.name == "public" {
				eqHex(t, "signed_bytes_sha256", sha(card.Env.SignedBytes()), hx(t, o, "signed_bytes_sha256"))
				eqHex(t, "sig", card.Env.Sig, hx(t, o, "sig"))
				id := card.Env.ID()
				eqHex(t, "object_id", id[:], hx(t, o, "object_id"))
				for _, a := range card.Addrs() {
					if a.GetSuite() == SuiteNoise3 {
						t.Fatal("noise3 address in the public card")
					}
				}
			}
			if c.name == "invite" {
				eqHex(t, "invite_id", card.Body.GetInviteId(), hx(t, o, "invite_id"))
				if card.Body.GetFlags()&(FlagUnlisted|FlagInviteScoped) != FlagUnlisted|FlagInviteScoped {
					t.Fatal("invite flags")
				}
			}
		})
	}
}

// The handshake initiator's cards: a user with its own account and device, and a throwaway identity.
func TestVectorInitiatorCards(t *testing.T) {
	v := loadVector(t, "node-card.json")
	hsAkeyOf := func(sk []byte) keys.AKey {
		k, err := ecdh.X25519().NewPrivateKey(sk)
		if err != nil {
			t.Fatal(err)
		}
		return keys.New(keys.X25519, k.PublicKey().Bytes())
	}
	seeds := get(t, v, "initiator", "seeds").(map[string]any)
	// The initiator uses the transcript's static key; the throwaway identity has its own (B4).
	hsKeys := map[string]keys.AKey{
		"initiator":           hsAkeyOf(sha([]byte("echofog v2 test vector seed: hs/xk/initiator_s"))),
		"initiator_throwaway": hsAkeyOf(hx(t, v, "initiator_throwaway", "hs_sk")),
	}
	eqHex(t, "throwaway hs seed", hx(t, v, "initiator_throwaway", "hs_sk"), unhex(t, seeds["initiator/throwaway_hs_sk"].(string)))
	if hsKeys["initiator"].Equal(hsKeys["initiator_throwaway"]) {
		t.Fatal("throwaway identity reuses the initiator's hs key")
	}
	for name, pre := range map[string]string{"initiator": "initiator/", "initiator_throwaway": "initiator/throwaway_"} {
		hsAkey := hsKeys[name]
		t.Run(name, func(t *testing.T) {
			acct := signer(t, seeds[pre+"account_seed"].(string))
			dev := signer(t, seeds[pre+"device_seed"].(string))
			dl, err := SealDeviceList(acct, fixedTime, fixedTime+30*day,
				&pb.DeviceList{Devices: []*pb.Device{{Key: dev.Public(), Added: fixedTime, Flags: DevicePrimary}}})
			if err != nil {
				t.Fatal(err)
			}
			body := &pb.NodeCard{
				ProtoVer: 200, HsKey: hsAkey, L1Suites: []uint32{1, 2},
				Services: []*pb.ServiceOffer{{Id: 3, Versions: []uint32{2}}, {Id: 7, Versions: []uint32{2}}},
				Features: []uint32{1, 3, 4, 5, 6}, Release: "0.3.0", Account: acct.Public(),
				Scope: uint32(ScopePeer), Devices: dl,
			}
			env, err := SealCard(dev, fixedTime, fixedTime+6*day, body)
			if err != nil {
				t.Fatal(err)
			}
			o := get(t, v, name).(map[string]any)
			eqHex(t, "envelope", env, hx(t, o, "envelope"))
			if uint64(len(env)) != num(t, o, "envelope_len") {
				t.Fatalf("envelope_len %d", len(env))
			}
			c, err := VerifyCard(env, vectorNow)
			if err != nil {
				t.Fatal(err)
			}
			eqHex(t, "node_id", c.NodeID[:], hx(t, o, "node_id"))
			eqHex(t, "device_id", c.DeviceID[:], hx(t, o, "device_id"))
			if c.Role() != RoleUser || c.Publishable() || len(c.Addrs()) != 0 {
				t.Fatal("initiator card properties")
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// link.json

func TestVectorLinks(t *testing.T) {
	cv := loadCardVector(t)
	lv := loadVector(t, "link.json")
	public := hx(t, cv.v, "public", "envelope")
	invite := hx(t, cv.v, "invite", "envelope")

	l, err := ParseLink(str(t, lv, "contact", "link"))
	if err != nil {
		t.Fatalf("ParseLink contact: %v", err)
	}
	eqHex(t, "contact card", l.Card, public)
	eqHex(t, "contact card sha", sha(l.Card), hx(t, lv, "contact", "card_envelope_sha256"))
	if l.Secret != nil || l.Method != 1 {
		t.Fatalf("contact link: secret %x method %d", l.Secret, l.Method)
	}
	if uint64(len(public)+1) != num(t, lv, "contact", "method0_len") {
		t.Fatal("method0_len")
	}
	if _, err := l.Verify(vectorNow); err != nil {
		t.Fatalf("contact Verify: %v", err)
	}

	l, err = ParseLink(str(t, lv, "invitation", "link"))
	if err != nil {
		t.Fatalf("ParseLink invitation: %v", err)
	}
	eqHex(t, "invite card", l.Card, invite)
	eqHex(t, "invite card sha", sha(l.Card), hx(t, lv, "invitation", "card_envelope_sha256"))
	eqHex(t, "access secret", l.Secret, hx(t, lv, "invitation", "access_secret"))
	pid := PassID(l.Secret)
	eqHex(t, "pass_id", pid[:], hx(t, lv, "invitation", "pass_id"))
	card, err := l.Verify(vectorNow)
	if err != nil {
		t.Fatalf("invitation Verify: %v", err)
	}
	if card.Scope != ScopeInvite {
		t.Fatal("invite scope")
	}

	w, err := ParseLink(str(t, lv, "invitation", "web_form"))
	if err != nil {
		t.Fatalf("ParseLink web form: %v", err)
	}
	if !bytes.Equal(w.Card, l.Card) || !bytes.Equal(w.Secret, l.Secret) {
		t.Fatal("web form differs")
	}
	if got := WebLink("example.org", str(t, lv, "invitation", "link")); got != str(t, lv, "invitation", "web_form") {
		t.Fatalf("WebLink %q", got)
	}

	// Encoding: DEFLATE bytes are informative; the encoder must round-trip and pick the shorter method.
	for _, env := range [][]byte{public, invite} {
		s, err := EncodeLink(env, nil)
		if err != nil {
			t.Fatal(err)
		}
		back, err := ParseLink(s)
		if err != nil || !bytes.Equal(back.Card, env) {
			t.Fatalf("round trip: %v", err)
		}
		if back.Method != 1 {
			t.Fatal("encoder did not pick the shorter (DEFLATE) form")
		}
	}
	s, _ := EncodeLink(public, nil)
	if s != str(t, lv, "contact", "link") {
		t.Logf("informative: contact link differs from the vector (compressor-dependent)")
	}
	s, _ = EncodeLink(invite, l.Secret)
	if !strings.HasSuffix(s, "&s="+str(t, lv, "invitation", "secret64")) {
		t.Fatalf("secret parameter: %q", s)
	}
}

// ---------------------------------------------------------------------------------------------
// negative.json

func TestVectorNegative(t *testing.T) {
	v := loadVector(t, "negative.json")
	s := testSigner(t, "negative")
	for _, e := range get(t, v, "non_canonical_bodies").([]any) {
		o := e.(map[string]any)
		body := unhex(t, o["body"].(string))
		if err := CheckCanonical(body, (&pb.NodeCard{}).ProtoReflect().Descriptor()); !errors.Is(err, ErrNonCanonical) {
			t.Fatalf("%s: CheckCanonical = %v", o["reason"], err)
		}
		// Correctly signed, still rejected.
		env := sealRaw(LabelCard, s, 1, fixedTime+3600, body)
		if _, err := Seal(LabelCard, s, 1, fixedTime+3600, body); !errors.Is(err, ErrNonCanonical) {
			t.Fatalf("%s: Seal = %v", o["reason"], err)
		}
		if _, err := Verify(env, vectorNow); !errors.Is(err, ErrNonCanonical) {
			t.Fatalf("%s: Verify = %v", o["reason"], err)
		}
		if _, err := VerifyCard(env, vectorNow); err == nil {
			t.Fatalf("%s: VerifyCard accepted", o["reason"])
		}
	}
	if _, err := Verify(hx(t, v, "signature_s_not_canonical", "envelope"), vectorNow); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("S >= L: %v", err)
	}
	if _, err := Verify(hx(t, v, "small_order_signer", "envelope"), vectorNow); !errors.Is(err, keys.ErrSmallOrder) {
		t.Fatalf("small-order signer: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// kdf.json: DHT keys of the record kinds (registries.md §9).

func TestVectorDHTKeys(t *testing.T) {
	v := loadVector(t, "kdf.json")
	day := uint32(num(t, v, "dht2", "day"))
	if day != uint32(fixedTime/86400) || DHTDay(vectorNow) != day {
		t.Fatalf("day %d", day)
	}
	var id [32]byte
	copy(id[:], hx(t, v, "dht2", "card_key", "signer_device_id"))
	k := DHTKey(LabelCard, id, day)
	eqHex(t, "card key", k[:], hx(t, v, "dht2", "card_key", "key"))
	copy(id[:], hx(t, v, "dht2", "devices_key", "signer_node_id"))
	k = DHTKey(LabelDevices, id, day)
	eqHex(t, "devices key", k[:], hx(t, v, "dht2", "devices_key", "key"))
	k = ReleaseDHTKey(day)
	eqHex(t, "release key", k[:], hx(t, v, "dht2", "release_key"))
}
