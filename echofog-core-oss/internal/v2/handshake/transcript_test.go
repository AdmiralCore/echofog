package handshake

import (
	"bytes"
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/mlkem/mlkemtest"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/noise"
)

const transcriptFile = "handshake-transcript.json"

// The transcript file. Every input is copied from handshake.json, node-card.json and link.json;
// the payloads are given as bytes so another implementation needs no protobuf encoder.

type tStep struct {
	Token string `json:"token"`
	H     string `json:"h"`
	CK    string `json:"ck"`
	K     string `json:"k"`
	N     uint64 `json:"n"`
}

type tMessage struct {
	Message        int      `json:"message"`
	Direction      string   `json:"direction"`
	Tokens         []string `json:"tokens"`
	Payload        string   `json:"payload"`
	HBeforePayload string   `json:"h_before_payload"`
	Len            int      `json:"len"`
	SHA256         string   `json:"sha256"`
	Bytes          string   `json:"bytes"`
	Steps          []tStep  `json:"steps"`
}

type tFinal struct {
	H             string `json:"h"`
	CK            string `json:"ck"`
	KI2R          string `json:"k_i2r"`
	KR2I          string `json:"k_r2i"`
	SessionSecret string `json:"session_secret"`
	SID           string `json:"sid"`
	Sealed        struct {
		Plaintext string `json:"plaintext"`
		N         uint64 `json:"n"`
		AAD       string `json:"aad"`
		Sealed    string `json:"sealed"`
		Record    string `json:"record"`
		Note      string `json:"note"`
	} `json:"sealed_record_example"`
}

type tRotation struct {
	Note      string `json:"note"`
	FirstPass struct {
		RS               string `json:"rs"`
		HAfterPremessage string `json:"h_after_premessage"`
		H1               string `json:"h1"`
		CKAfterES        string `json:"ck_after_es"`
		KAfterES         string `json:"k_after_es"`
		P1Open           string `json:"p1_open"`
	} `json:"first_pass"`
	SecondPass struct {
		RS     string `json:"rs"`
		H1     string `json:"h1"`
		P1Open string `json:"p1_open"`
	} `json:"second_pass"`
}

type tInputs struct {
	Prologue           string `json:"prologue"`
	PrologueSource     string `json:"prologue_source"`
	InitiatorStaticSK  string `json:"initiator_s_sk"`
	InitiatorStaticPub string `json:"initiator_s_pub"`
	ResponderStaticSK  string `json:"responder_s_sk"`
	ResponderStaticPub string `json:"responder_s_pub"`
	InitiatorESK       string `json:"initiator_e_sk"`
	InitiatorEPub      string `json:"initiator_e_pub"`
	ResponderESK       string `json:"responder_e_sk"`
	ResponderEPub      string `json:"responder_e_pub"`
	E1SeedDZ           string `json:"initiator_e1_seed_dz"`
	E1EKSHA256         string `json:"initiator_e1_ek_sha256"`
	EncapsM            string `json:"encaps_randomness_m"`
	CTSHA256           string `json:"ekem1_ct_sha256"`
	SS                 string `json:"ekem1_ss"`
	P1                 string `json:"p1"`
	P1Source           string `json:"p1_source"`
	P2                 string `json:"p2"`
	P2Source           string `json:"p2_source"`
	P3                 string `json:"p3"`
	P3Source           string `json:"p3_source"`
}

type tTranscript struct {
	Name         string     `json:"name"`
	SuiteID      uint16     `json:"suite_id"`
	ProtocolName string     `json:"protocol_name"`
	Pattern      []string   `json:"pattern"`
	Inputs       tInputs    `json:"inputs"`
	Initialize   []tStep    `json:"initialize"`
	Messages     []tMessage `json:"messages"`
	Final        tFinal     `json:"final"`
	Checks       []string   `json:"checks,omitempty"`
}

type tFile struct {
	Description string        `json:"description"`
	Spec        string        `json:"spec"`
	Conventions []string      `json:"conventions"`
	Transcripts []tTranscript `json:"transcripts"`
	Rotation    *tRotation    `json:"rotation,omitempty"`
}

type transcriptInputs struct {
	pattern              *noise.Pattern
	suite                uint16
	name                 string
	prologue             []byte
	prologueSource       string
	is, rs, ie, re       *ecdh.PrivateKey
	seed, m              []byte
	p1, p2, p3           []byte
	p1Src, p2Src, p3Src  string
	p1Func               func(h []byte) []byte // overrides p1 when set (access proof over the real h1)
	checks               []string
	ekHash, ctHash, ssHx []byte
}

func hx(b []byte) string { return hex.EncodeToString(b) }

func patternTokens(p *noise.Pattern) []string {
	var out []string
	if p.PreResponderStatic {
		out = append(out, "<- s", "...")
	}
	for i, m := range p.Messages {
		dir := "->"
		if i%2 == 1 {
			dir = "<-"
		}
		s := dir
		for j, t := range m {
			if j == 0 {
				s += " " + t.String()
			} else {
				s += ", " + t.String()
			}
		}
		out = append(out, s)
	}
	return out
}

func buildTranscript(t testing.TB, in transcriptInputs) tTranscript {
	t.Helper()
	var wTrace, rTrace []noise.TraceEvent
	var gotCT, gotSS []byte
	encap := func(ek *mlkem.EncapsulationKey768) ([]byte, []byte, error) {
		ss, ct, err := mlkemtest.Encapsulate768(ek, in.m)
		gotCT, gotSS = ct, append([]byte(nil), ss...)
		return ss, ct, err
	}
	ic := noise.Config{Pattern: in.pattern, Initiator: true, Prologue: in.prologue, Static: in.is,
		Ephemeral: in.ie, KEMSeed: in.seed, Trace: func(e noise.TraceEvent) { wTrace = append(wTrace, e) }}
	if in.pattern.PreResponderStatic {
		ic.RemoteStatic = in.rs.PublicKey().Bytes()
	}
	ini, err := noise.NewHandshake(ic)
	if err != nil {
		t.Fatal(err)
	}
	res, err := noise.NewHandshake(noise.Config{Pattern: in.pattern, Prologue: in.prologue, Static: in.rs,
		Ephemeral: in.re, KEMEncapsulate: encap, Trace: func(e noise.TraceEvent) { rTrace = append(rTrace, e) }})
	if err != nil {
		t.Fatal(err)
	}
	step := func(e noise.TraceEvent) tStep {
		return tStep{Token: e.Token, H: hx(e.H), CK: hx(e.CK), K: hx(e.K), N: e.N}
	}
	var init []tStep
	for _, e := range wTrace {
		init = append(init, step(e))
	}
	if len(rTrace) != len(wTrace) {
		t.Fatal("initialisation traces differ")
	}
	for i := range wTrace {
		if !reflect.DeepEqual(wTrace[i], rTrace[i]) {
			t.Fatalf("initialisation step %d differs", i)
		}
	}
	tr := tTranscript{Name: in.name, SuiteID: in.suite, ProtocolName: in.pattern.ProtocolName,
		Pattern: patternTokens(in.pattern), Initialize: init, Checks: in.checks}
	p1 := in.p1
	payloads := [3]func(h []byte) []byte{
		func(h []byte) []byte {
			if in.p1Func != nil {
				p1 = in.p1Func(h)
			}
			return p1
		},
		func([]byte) []byte { return in.p2 },
		func([]byte) []byte { return in.p3 },
	}
	sides := []struct {
		w, r *noise.HandshakeState
		dir  string
	}{{ini, res, "->"}, {res, ini, "<-"}, {ini, res, "->"}}
	for n, s := range sides {
		wTrace, rTrace = nil, nil
		var pl []byte
		msg, err := s.w.WriteMessageFunc(nil, func(h []byte) ([]byte, error) {
			pl = payloads[n](h)
			return pl, nil
		})
		if err != nil {
			t.Fatalf("%s: write %d: %v", in.name, n+1, err)
		}
		got, err := s.r.ReadMessage(msg)
		if err != nil {
			t.Fatalf("%s: read %d: %v", in.name, n+1, err)
		}
		if !bytes.Equal(got, pl) {
			t.Fatalf("%s: payload %d", in.name, n+1)
		}
		// The two sides must have passed through identical states.
		wt, rt := wTrace, rTrace
		if n%2 == 1 {
			wt, rt = rTrace, wTrace
		}
		if len(wt) != len(rt) {
			t.Fatalf("%s: message %d: traces differ in length", in.name, n+1)
		}
		var steps []tStep
		var toks []string
		for i := range wt {
			if !reflect.DeepEqual(wt[i], rt[i]) {
				t.Fatalf("%s: message %d step %s: writer and reader differ", in.name, n+1, wt[i].Token)
			}
			steps = append(steps, step(wt[i]))
			if wt[i].Token != "payload" {
				toks = append(toks, wt[i].Token)
			}
		}
		sum := sha256.Sum256(msg)
		tr.Messages = append(tr.Messages, tMessage{Message: n + 1, Direction: s.dir, Tokens: toks, Payload: hx(pl),
			HBeforePayload: hx(s.w.HashBeforePayload()), Len: len(msg), SHA256: hx(sum[:]), Bytes: hx(msg), Steps: steps})
		if !bytes.Equal(s.w.HashBeforePayload(), s.r.HashBeforePayload()) {
			t.Fatal("h before payload differs")
		}
	}
	if !bytes.Equal(ini.H(), res.H()) || !bytes.Equal(ini.CK(), res.CK()) {
		t.Fatal("final state differs")
	}
	i2r, r2i, err := ini.Split()
	if err != nil {
		t.Fatal(err)
	}
	ri2r, rr2i, _ := res.Split()
	if !bytes.Equal(i2r.Key(), ri2r.Key()) || !bytes.Equal(r2i.Key(), rr2i.Key()) {
		t.Fatal("split differs")
	}
	sec := SessionSecret(ini.CK())
	sid := SessionID(sec, ini.H())
	tr.Final = tFinal{H: hx(ini.H()), CK: hx(ini.CK()), KI2R: hx(i2r.Key()), KR2I: hx(r2i.Key()),
		SessionSecret: hx(sec), SID: hx(sid)}
	// wire.md §8.4: one sealed record (class 1, path 0, pipe 0) carrying a MUX_STREAM frame on
	// stream 4. The record format belongs to the frame layer; it is built here only as a check of
	// the transport keys.
	data := []byte("hello over a sealed path")
	pt := append([]byte{0x18, 0x00, byte(2 + len(data)), 0x04, 0x00}, data...)
	aad := append(append(append([]byte(nil), sid...), be64(0)...), 1, 0, 0)
	ct, err := i2r.EncryptWithAd(nil, aad, pt)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := ri2r.DecryptWithAd(nil, aad, ct); err != nil || !bytes.Equal(back, pt) {
		t.Fatal("sealed record example")
	}
	sealed := append(be64(0), ct...)
	record := append([]byte{byte((1 + len(sealed)) >> 8), byte(1 + len(sealed)), 1}, sealed...)
	tr.Final.Sealed.Plaintext, tr.Final.Sealed.AAD = hx(pt), hx(aad)
	tr.Final.Sealed.Sealed, tr.Final.Sealed.Record = hx(sealed), hx(record)

	ekHash := sha256.Sum256(func() []byte {
		dk, _ := mlkem.NewDecapsulationKey768(in.seed)
		return dk.EncapsulationKey().Bytes()
	}())
	ctHash := sha256.Sum256(gotCT)
	if !bytes.Equal(ekHash[:], in.ekHash) || !bytes.Equal(ctHash[:], in.ctHash) || !bytes.Equal(gotSS, in.ssHx) {
		t.Fatalf("%s: ML-KEM values disagree with handshake.json", in.name)
	}
	tr.Inputs = tInputs{
		Prologue: hx(in.prologue), PrologueSource: in.prologueSource,
		InitiatorStaticSK: hx(in.is.Bytes()), InitiatorStaticPub: hx(in.is.PublicKey().Bytes()),
		ResponderStaticSK: hx(in.rs.Bytes()), ResponderStaticPub: hx(in.rs.PublicKey().Bytes()),
		InitiatorESK: hx(in.ie.Bytes()), InitiatorEPub: hx(in.ie.PublicKey().Bytes()),
		ResponderESK: hx(in.re.Bytes()), ResponderEPub: hx(in.re.PublicKey().Bytes()),
		E1SeedDZ: hx(in.seed), E1EKSHA256: hx(ekHash[:]), EncapsM: hx(in.m), CTSHA256: hx(ctHash[:]), SS: hx(gotSS),
		P1: hx(p1), P1Source: in.p1Src, P2: hx(in.p2), P2Source: in.p2Src, P3: hx(in.p3), P3Source: in.p3Src,
	}
	return tr
}

func xKey(t testing.TB, b []byte) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func buildTranscriptFile(t testing.TB) tFile {
	t.Helper()
	hsv := loadVector(t, "handshake.json")
	card := loadVector(t, "node-card.json")
	link := loadVector(t, "link.json")
	in := obj(t, hsv, "transcript_inputs")
	pay := obj(t, hsv, "payloads")
	secret := hexField(t, obj(t, link, "invitation"), "access_secret")

	base := transcriptInputs{
		is:     xKey(t, hexField(t, in, "initiator_s_sk")),
		rs:     xKey(t, hexField(t, card, "hs_sk")),
		ie:     xKey(t, hexField(t, in, "initiator_e_sk")),
		re:     xKey(t, hexField(t, in, "responder_e_sk")),
		seed:   hexField(t, in, "initiator_e1_seed_dz"),
		m:      hexField(t, in, "encaps_randomness_m"),
		ekHash: hexField(t, in, "initiator_e1_ek_sha256"),
		ctHash: hexField(t, in, "ekem1_ct_sha256"),
		ssHx:   hexField(t, in, "ekem1_ss"),
	}
	for _, c := range []struct{ got, want []byte }{
		{base.is.PublicKey().Bytes(), hexField(t, in, "initiator_s_pub")},
		{base.ie.PublicKey().Bytes(), hexField(t, in, "initiator_e_pub")},
		{base.re.PublicKey().Bytes(), hexField(t, in, "responder_e_pub")},
		{base.rs.PublicKey().Bytes(), hexField(t, obj(t, hsv, "prologue_xk"), "responder_static_rs")},
	} {
		if !bytes.Equal(c.got, c.want) {
			t.Fatal("handshake.json public keys disagree")
		}
	}
	enc := func(name string) []byte { return hexField(t, obj(t, pay, name), "encoded") }
	p1, err := ParseHsInit(enc("HsInit_xk"))
	if err != nil {
		t.Fatal(err)
	}
	inviteSeq := num(t, obj(t, card, "invite"), "seq")

	xk := base
	xk.pattern, xk.suite, xk.name = noise.XKhfs, SuiteXK, "XKhfs"
	xk.prologue = hexField(t, obj(t, hsv, "prologue_xk"), "prologue")
	xk.prologueSource = "handshake.json prologue_xk.prologue"
	xk.p1, xk.p1Src = enc("HsInit_xk"), "handshake.json payloads.HsInit_xk.encoded"
	xk.p2, xk.p2Src = enc("HsResp"), "handshake.json payloads.HsResp.encoded"
	xk.p3, xk.p3Src = enc("HsFinish"), "handshake.json payloads.HsFinish.encoded"

	xkA := xk
	xkA.name = "XKhfs-access-proof"
	xkA.prologue = hexField(t, obj(t, hsv, "prologue_xk_hidden"), "prologue")
	xkA.prologueSource = "handshake.json prologue_xk_hidden.prologue"
	xkA.p1 = nil
	xkA.p1Func = func(h1 []byte) []byte {
		m := *p1
		m.KnownCardSeq = inviteSeq
		m.Access = &Access{PassID: PassID(secret), Proof: AccessProof(secret, h1)}
		return m.Marshal()
	}
	xkA.p1Src = "payloads.HsInit_xk_hidden: access proof over messages[0].h_before_payload"

	xx := base
	xx.is = xKey(t, hexField(t, obj(t, card, "initiator_throwaway"), "hs_sk")) // B4: its own hs key
	xx.pattern, xx.suite, xx.name = noise.XXhfs, SuiteXX, "XXhfs"
	xx.prologue = hexField(t, obj(t, hsv, "prologue_xx_bare_address"), "prologue")
	xx.prologueSource = "handshake.json prologue_xx_bare_address.prologue"
	xx.p1, xx.p1Src = enc("HsInit_xx"), "handshake.json payloads.HsInit_xx.encoded"
	xx.p2, xx.p2Src = enc("HsResp_xx"), "handshake.json payloads.HsResp_xx.encoded"
	xx.p3, xx.p3Src = enc("HsFinish_xx"), "handshake.json payloads.HsFinish_xx.encoded"

	f := tFile{
		Description: "XKhfs and XXhfs transcripts recomputed by internal/v2/handshake from handshake.json, node-card.json and link.json (for comparison with the normative handshake-transcript.json).",
		Spec:        "handshake.md §12",
	}
	// XKhfs-rotation-prev-hs: the initiator's card copy names the previous hs key.
	prev := xKey(t, hexField(t, obj(t, card, "seeds"), "card/prev_hs_sk"))
	if !bytes.Equal(append([]byte{AlgX25519}, prev.PublicKey().Bytes()...), hexField(t, card, "prev_hs_akey")) {
		t.Fatal("prev_hs key")
	}
	rot := xkA
	rot.name, rot.rs = "XKhfs-rotation-prev-hs", prev
	for _, in := range []transcriptInputs{xk, xkA, xx, rot} {
		f.Transcripts = append(f.Transcripts, buildTranscript(t, in))
	}
	f.Rotation = buildRotation(t, f.Transcripts[3], base.rs, prev)
	// The hidden transcript's P1 must be exactly handshake.json HsInit_xk_hidden.
	if f.Transcripts[1].Inputs.P1 != hx(enc("HsInit_xk_hidden")) {
		t.Fatal("recomputed HsInit_xk_hidden differs from handshake.json")
	}
	return f
}

// TestTranscriptVectors recomputes the three transcripts and compares every value with the
// normative vectors/handshake-transcript.json (descriptions and source notes excepted). With
// HS_TRANSCRIPT_OUT set, it also writes the recomputed file there (for gen -crosscheck).
func TestTranscriptVectors(t *testing.T) {
	got := buildTranscriptFile(t)
	if out := os.Getenv("HS_TRANSCRIPT_OUT"); out != "" {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(got); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(vectorDir, transcriptFile))
	if err != nil {
		t.Fatal(err)
	}
	var want tFile
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want.Transcripts) != len(got.Transcripts) {
		t.Fatalf("%d transcripts, want %d", len(got.Transcripts), len(want.Transcripts))
	}
	for n := range want.Transcripts {
		w, g := want.Transcripts[n], got.Transcripts[n]
		for _, x := range []*tTranscript{&w, &g} {
			x.Checks, x.Final.Sealed.Note = nil, ""
			x.Inputs.PrologueSource, x.Inputs.P1Source, x.Inputs.P2Source, x.Inputs.P3Source = "", "", "", ""
		}
		if w.Name != g.Name || w.SuiteID != g.SuiteID || w.ProtocolName != g.ProtocolName || !reflect.DeepEqual(w.Pattern, g.Pattern) {
			t.Fatalf("%s: header differs", w.Name)
		}
		if !reflect.DeepEqual(w.Inputs, g.Inputs) {
			t.Errorf("%s: inputs differ:\n got %+v\nwant %+v", w.Name, g.Inputs, w.Inputs)
		}
		if !reflect.DeepEqual(w.Initialize, g.Initialize) {
			t.Errorf("%s: initialisation differs", w.Name)
		}
		for i := range w.Messages {
			if !reflect.DeepEqual(w.Messages[i], g.Messages[i]) {
				t.Errorf("%s: message %d differs", w.Name, i+1)
			}
		}
		if !reflect.DeepEqual(w.Final, g.Final) {
			t.Errorf("%s: final values differ:\n got %+v\nwant %+v", w.Name, g.Final, w.Final)
		}
	}
	if want.Rotation == nil || got.Rotation == nil {
		t.Fatal("rotation block missing")
	}
	w, g := *want.Rotation, *got.Rotation
	w.Note, w.FirstPass.P1Open, w.SecondPass.P1Open = "", "", ""
	if w != g {
		t.Errorf("rotation differs: got %+v want %+v", g, w)
	}
}

// buildRotation replays message 1 of the rotation transcript at a responder whose current key is
// cur: the first pass must fail, the second with prev must succeed (handshake.md §4.1).
func buildRotation(t testing.TB, tr tTranscript, cur, prev *ecdh.PrivateKey) *tRotation {
	t.Helper()
	m1 := mustHex(t, tr.Messages[0].Bytes)
	pro := mustHex(t, tr.Inputs.Prologue)
	r := &tRotation{}
	var steps []noise.TraceEvent
	hs, err := noise.NewHandshake(noise.Config{Pattern: noise.XKhfs, Prologue: pro, Static: cur,
		Trace: func(e noise.TraceEvent) { steps = append(steps, e) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hs.ReadMessage(m1); err == nil {
		t.Fatal("rotation: the current key opened P1")
	}
	for _, e := range steps {
		switch e.Token {
		case "pre:s":
			r.FirstPass.HAfterPremessage = hx(e.H)
		case "es":
			r.FirstPass.CKAfterES, r.FirstPass.KAfterES, r.FirstPass.H1 = hx(e.CK), hx(e.K), hx(e.H)
		}
	}
	r.FirstPass.RS = hx(cur.PublicKey().Bytes())
	hs2, err := noise.NewHandshake(noise.Config{Pattern: noise.XKhfs, Prologue: pro, Static: prev})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hs2.ReadMessage(m1); err != nil {
		t.Fatalf("rotation: previous key: %v", err)
	}
	r.SecondPass.RS, r.SecondPass.H1 = hx(prev.PublicKey().Bytes()), hx(hs2.HashBeforePayload())
	return r
}
