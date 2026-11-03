package handshake

import (
	"bytes"
	"reflect"
	"testing"
)

func TestHelloVectors(t *testing.T) {
	v := loadVector(t, "handshake.json")
	cases := []struct {
		name string
		hex  string
		want Hello
	}{
		{"xk", str(t, obj(t, v, "prologue_xk"), "hs_hello"), Hello{Offered: []uint16{1, 2}, Chosen: 1, Flags: FlagResponderID}},
		{"xx", str(t, obj(t, v, "prologue_xx_bare_address"), "hs_hello"), Hello{Offered: []uint16{2}, Chosen: 2}},
		{"xk hidden", str(t, obj(t, v, "prologue_xk_hidden"), "hs_hello"), Hello{Offered: []uint16{1, 0x0a0a}, Chosen: 1, Flags: FlagResponderID}},
		{"puzzle", str(t, obj(t, v, "puzzle"), "HS_HELLO_with_solution_payload"), Hello{Offered: []uint16{1, 2}, Chosen: 1,
			Flags: FlagPuzzle | FlagResponderID, Cookie: mustHex(t, "9dc93509d0bb2bb8c9ed254591c07fab"), Nonce: 2274}},
	}
	f := loadVector(t, "frames.json")
	for _, fr := range f["frames"].([]any) {
		m := fr.(map[string]any)
		if m["name"] == "HS_HELLO (puzzle)" {
			cases = append(cases, struct {
				name string
				hex  string
				want Hello
			}{"frames.json puzzle", str(t, m, "payload"), Hello{Offered: []uint16{1, 2}, Chosen: 1, Flags: 3,
				Cookie: mustHex(t, "a0a1a2a3a4a5a6a7a8a9aaabacadaeaf"), Nonce: 4242}})
		}
	}
	for _, c := range cases {
		b := mustHex(t, c.hex)
		got, err := c.want.Marshal()
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s: marshal = %x (%v), want %x", c.name, got, err, b)
		}
		p, err := ParseHello(b)
		if err != nil {
			t.Fatalf("%s: parse: %v", c.name, err)
		}
		if !reflect.DeepEqual(*p, c.want) {
			t.Errorf("%s: parse = %+v, want %+v", c.name, *p, c.want)
		}
	}
}

func TestHelloInvalid(t *testing.T) {
	bad := []struct {
		name string
		b    string
	}{
		{"empty", ""},
		{"n=0", "00000000"},
		{"n=9", "09" + "0001000200030004000500060007000800090001" + "00"},
		{"chosen not offered", "0100010002" + "00"},
		{"truncated", "02000100"},
		{"puzzle flag without solution", "0200010002000101"},
		{"trailing byte", "020001000200010200"},
		{"duplicate offered", "020001000100010000"},
		{"GREASE chosen", "0200010a0a0a0a00"},
	}
	for _, c := range bad {
		if _, err := ParseHello(mustHex(t, c.b)); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	// Reserved flag bits are ignored on receipt.
	h, err := ParseHello(mustHex(t, "0200010002000182"))
	if err != nil || h.Flags&FlagResponderID == 0 {
		t.Fatalf("reserved flag bit: %v", err)
	}
}

func TestPrologueVectors(t *testing.T) {
	v := loadVector(t, "handshake.json")
	xk := obj(t, v, "prologue_xk")
	got := Prologue(hexField(t, xk, "cb"), hexField(t, xk, "rid (node-card.json device_id)"), hexField(t, xk, "hs_hello"))
	if want := hexField(t, xk, "prologue"); !bytes.Equal(got, want) {
		t.Errorf("XK prologue = %x, want %x", got, want)
	}
	hid := obj(t, v, "prologue_xk_hidden")
	got = Prologue(hexField(t, xk, "cb"), hexField(t, xk, "rid (node-card.json device_id)"), hexField(t, hid, "hs_hello"))
	if want := hexField(t, hid, "prologue"); !bytes.Equal(got, want) {
		t.Errorf("XK hidden prologue = %x, want %x", got, want)
	}
	xx := obj(t, v, "prologue_xx_bare_address")
	got = Prologue(hexField(t, xk, "cb"), nil, hexField(t, xx, "hs_hello"))
	if want := hexField(t, xx, "prologue"); !bytes.Equal(got, want) {
		t.Errorf("XX prologue = %x, want %x", got, want)
	}
}

func TestPayloadVectors(t *testing.T) {
	v := loadVector(t, "handshake.json")
	p := obj(t, v, "payloads")
	card := loadVector(t, "node-card.json")

	services := []ServiceOffer{}
	for id := uint32(1); id <= 8; id++ {
		services = append(services, ServiceOffer{ID: id, Versions: []uint32{2}})
	}
	services = append(services, ServiceOffer{ID: 0x3a3a, Versions: []uint32{1}})
	base := HsInit{ProtoVer: 200, Features: []uint32{1, 3, 4, 5, 0x2a2a}, Services: services, Time: fixedTime}
	xk := base
	xk.KnownCardSeq = num(t, obj(t, card, "public"), "seq")
	resp := HsResp{
		ProtoVer: 200,
		Features: []uint32{1, 3, 4, 5},
		Services: []ServiceChoice{{1, 2}, {2, 2}, {3, 2}},
		Observed: []byte{4, 192, 0, 2, 33, 0xc8, 0x22},
		Time:     fixedTime,
	}
	respXX := resp
	respXX.Card = hexField(t, obj(t, card, "public"), "envelope")

	check := func(name string, marshal func() []byte, parse func([]byte) (any, error), want any) {
		t.Helper()
		enc := hexField(t, obj(t, p, name), "encoded")
		if got := marshal(); !bytes.Equal(got, enc) {
			t.Errorf("%s = %x, want %x", name, got, enc)
		}
		dec, err := parse(enc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(dec, want) {
			t.Errorf("%s parse = %+v", name, dec)
		}
	}
	pInit := func(b []byte) (any, error) { m, err := ParseHsInit(b); return m, err }
	pResp := func(b []byte) (any, error) { m, err := ParseHsResp(b); return m, err }
	pFin := func(b []byte) (any, error) { m, err := ParseHsFinish(b); return m, err }
	check("HsInit_xk", xk.Marshal, pInit, &xk)
	check("HsInit_xx", base.Marshal, pInit, &base)
	check("HsResp", resp.Marshal, pResp, &resp)
	check("HsResp_xx", respXX.Marshal, pResp, &respXX)
	fin := HsFinish{Card: hexField(t, obj(t, card, "initiator"), "envelope")}
	check("HsFinish", fin.Marshal, pFin, &fin)
	finXX := HsFinish{Card: hexField(t, obj(t, card, "initiator_throwaway"), "envelope")}
	check("HsFinish_xx", finXX.Marshal, pFin, &finXX)

	// HsInit_xk_hidden: as HsInit_xk with the invitation card's seq and an access proof over the
	// transcript's h1 (checked against the transcript in TestApplicationTranscripts).
	hid, err := ParseHsInit(hexField(t, obj(t, p, "HsInit_xk_hidden"), "encoded"))
	if err != nil {
		t.Fatal(err)
	}
	link := loadVector(t, "link.json")
	if hid.Access == nil || !bytes.Equal(hid.Access.PassID, hexField(t, obj(t, link, "invitation"), "pass_id")) ||
		hid.KnownCardSeq != num(t, obj(t, card, "invite"), "seq") {
		t.Fatal("HsInit_xk_hidden fields")
	}
	hid.Access, hid.KnownCardSeq = nil, xk.KnownCardSeq
	if !reflect.DeepEqual(*hid, xk) {
		t.Fatal("HsInit_xk_hidden differs from HsInit_xk beyond access and known_card_seq")
	}
}

func TestRejectVector(t *testing.T) {
	f := loadVector(t, "frames.json")
	for _, fr := range f["frames"].([]any) {
		m := fr.(map[string]any)
		if m["name"] != "REJECT" {
			continue
		}
		want := mustHex(t, str(t, m, "payload"))
		r := Reject{Reason: ReasonVersion, Min: 200, Max: 200}
		if got := r.Marshal(); !bytes.Equal(got, want) {
			t.Fatalf("Reject = %x, want %x", got, want)
		}
		d, err := ParseReject(want)
		if err != nil || *d != r {
			t.Fatalf("Reject parse = %+v %v", d, err)
		}
		return
	}
	t.Fatal("no REJECT vector")
}

func TestAccessVectors(t *testing.T) {
	v := loadVector(t, "handshake.json")
	a := obj(t, v, "access_example")
	s := hexField(t, a, "secret (link.json)")
	if got := PassID(s); !bytes.Equal(got, hexField(t, a, "pass_id")) {
		t.Errorf("pass_id = %x", got)
	}
	if got := AccessProof(s, hexField(t, a, "h1")); !bytes.Equal(got, hexField(t, a, "proof")) {
		t.Errorf("proof = %x", got)
	}
	link := loadVector(t, "link.json")
	inv := obj(t, link, "invitation")
	if !bytes.Equal(hexField(t, inv, "access_secret"), s) || !bytes.Equal(hexField(t, inv, "pass_id"), PassID(s)) {
		t.Error("link.json secret/pass_id disagree")
	}
}

func TestPuzzleVectors(t *testing.T) {
	v := loadVector(t, "handshake.json")
	pz := obj(t, v, "puzzle")
	cb := hexField(t, obj(t, v, "prologue_xk"), "cb")
	minute := num(t, pz, "minute")
	if minute != fixedTime/60 {
		t.Fatal("minute")
	}
	cookie := Cookie(hexField(t, pz, "cookie_secret_R"), cb, minute)
	if !bytes.Equal(cookie, hexField(t, pz, "cookie")) {
		t.Fatalf("cookie = %x", cookie)
	}
	nonce := num(t, pz, "nonce")
	if got := PuzzleHash(cookie, cb, nonce); !bytes.Equal(got, hexField(t, pz, "hash")) {
		t.Fatalf("hash = %x", got)
	}
	d := uint8(num(t, pz, "difficulty"))
	if got, err := SolvePuzzle(cookie, cb, d); err != nil || got != nonce {
		t.Fatalf("solve = %d %v, want %d", got, err, nonce)
	}
	if !PuzzleOK(cookie, cb, nonce, d) || PuzzleOK(cookie, cb, nonce-1, d) {
		t.Fatal("PuzzleOK")
	}
	frame := hexField(t, pz, "HS_RETRY_frame")
	// frame = type 0x04 ‖ flags 0 ‖ len 0x11 ‖ payload
	if frame[0] != FrameRetry || frame[1] != 0 || int(frame[2]) != len(frame)-3 {
		t.Fatal("HS_RETRY frame header")
	}
	r := Retry{Difficulty: d}
	copy(r.Cookie[:], cookie)
	if got := r.Marshal(); !bytes.Equal(got, frame[3:]) {
		t.Fatalf("HS_RETRY payload = %x", got)
	}
	pr, err := ParseRetry(frame[3:])
	if err != nil || *pr != r {
		t.Fatalf("ParseRetry %v", err)
	}
	if _, err := ParseRetry(append(frame[3:len(frame)-1:len(frame)-1], 25)); err == nil {
		t.Fatal("difficulty 25 accepted")
	}
	if _, err := SolvePuzzle(cookie, cb, 25); err == nil {
		t.Fatal("solve with difficulty 25")
	}
}

func TestSessionDerivationVectors(t *testing.T) {
	v := loadVector(t, "kdf.json")
	s := obj(t, v, "session")
	sec := SessionSecret(hexField(t, s, "ck"))
	if !bytes.Equal(sec, hexField(t, s, "session_secret (noise HKDF(ck, label).o1)")) {
		t.Fatalf("session secret = %x", sec)
	}
	if sid := SessionID(sec, hexField(t, s, "h")); !bytes.Equal(sid, hexField(t, s, "sid")) {
		t.Fatalf("sid = %x", sid)
	}
}

func TestCircuitBindingVector(t *testing.T) {
	v := loadVector(t, "kdf.json")
	r := obj(t, v, "relay2")
	got := CircuitBinding(hexField(t, r, "relay_node_id"), 0x0123456789abcdef)
	if !bytes.Equal(got, hexField(t, r, "cb_circuit")) {
		t.Fatalf("cb_circuit = %x", got)
	}
}

func TestSuiteNames(t *testing.T) {
	v := loadVector(t, "handshake.json")
	for _, e := range v["initialize_symmetric"].([]any) {
		m := e.(map[string]any)
		id := uint16(num(t, m, "suite_id"))
		s, ok := SuiteByID(id)
		if !ok || s.Name != str(t, m, "protocol_name") {
			t.Fatalf("suite %d", id)
		}
	}
	for _, g := range []uint16{0x0a0a, 0x1a1a, 0x7a7a, 0xeaea} {
		if !IsGrease(g) {
			t.Fatalf("%04x is GREASE", g)
		}
	}
	for _, g := range []uint16{0xfafa, 0x0001, 0x0a0b, 0x2a2b} {
		if IsGrease(g) {
			t.Fatalf("%04x is not GREASE", g)
		}
	}
}
