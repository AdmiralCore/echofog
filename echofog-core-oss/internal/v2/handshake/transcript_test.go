package handshake

import (
	"bytes"
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/mlkem/mlkemtest"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/noise"
)

var update = flag.Bool("update", false, "rewrite vectors/handshake-transcript.json")

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
	SealedExample struct {
		Plaintext  string `json:"plaintext"`
		Nonce      uint64 `json:"n"`
		AD         string `json:"ad"`
		Ciphertext string `json:"ciphertext"`
		Note       string `json:"note"`
	} `json:"transport_example"`
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
	pt := []byte("hello over a sealed path")
	ad := sid
	ct, err := i2r.EncryptWithAd(nil, ad, pt)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := ri2r.DecryptWithAd(nil, ad, ct); err != nil || !bytes.Equal(back, pt) {
		t.Fatal("transport example")
	}
	tr.Final.SealedExample.Plaintext = hx(pt)
	tr.Final.SealedExample.AD = hx(ad)
	tr.Final.SealedExample.Ciphertext = hx(ct)
	tr.Final.SealedExample.Note = "ENCRYPT(k_i2r, n = 0, ad = sid, plaintext): a check of the transport keys and nonce format only, not a wire.md §8.4 record"

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

func buildTranscriptFile(t testing.TB) []byte {
	t.Helper()
	hsv := loadVector(t, "handshake.json")
	card := loadVector(t, "node-card.json")
	link := loadVector(t, "link.json")
	in := obj(t, hsv, "deferred_transcripts", "inputs_xk")
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
	hsInit := hexField(t, obj(t, pay, "HsInit"), "encoded")
	hsResp := hexField(t, obj(t, pay, "HsResp"), "encoded")
	peer := hexField(t, obj(t, card, "peer"), "envelope")
	public := hexField(t, obj(t, card, "public"), "envelope")
	hsFinish := (&HsFinish{Card: peer}).Marshal()
	sum := sha256.Sum256(hsFinish)
	if !bytes.Equal(sum[:], hexField(t, obj(t, pay, "HsFinish"), "encoded_sha256")) {
		t.Fatal("HsFinish does not match handshake.json")
	}
	p1, err := ParseHsInit(hsInit)
	if err != nil {
		t.Fatal(err)
	}

	// 1. XKhfs exactly as handshake.json deferred_transcripts.inputs_xk says.
	xk := base
	xk.pattern, xk.suite, xk.name = noise.XKhfs, SuiteXK, "XKhfs"
	xk.prologue = hexField(t, obj(t, hsv, "prologue_xk"), "prologue")
	xk.prologueSource = "handshake.json prologue_xk.prologue"
	xk.p1, xk.p1Src = hsInit, "handshake.json payloads.HsInit.encoded (its access proof is over the fixed seed hs/h1, not over this transcript's h1)"
	xk.p2, xk.p2Src = hsResp, "handshake.json payloads.HsResp.encoded"
	xk.p3, xk.p3Src = hsFinish, "HsFinish{card = node-card.json peer.envelope}; SHA-256 = handshake.json payloads.HsFinish.encoded_sha256"
	xk.checks = []string{"pure Noise transcript: payloads are opaque, card and access checks are not applied"}

	// 2. XKhfs with P1's access proof recomputed over this transcript's real h1.
	xkA := xk
	xkA.name = "XKhfs-access-proof"
	xkA.p1 = nil
	xkA.p1Func = func(h1 []byte) []byte {
		m := *p1
		m.Access = &Access{PassID: PassID(secret), Proof: AccessProof(secret, h1)}
		return m.Marshal()
	}
	xkA.p1Src = "payloads.HsInit with access.proof = HMAC-SHA256(link.json invitation.access_secret, \"echofog/hs2/access-proof\" ‖ messages[0].h_before_payload); all other fields unchanged"
	xkA.checks = []string{"a responder holding link.json's access secret accepts P1.access of this transcript"}

	// 3. XXhfs to a bare address (handshake.json prologue_xx_bare_address).
	xx := base
	xx.pattern, xx.suite, xx.name = noise.XXhfs, SuiteXX, "XXhfs"
	xx.prologue = hexField(t, obj(t, hsv, "prologue_xx_bare_address"), "prologue")
	xx.prologueSource = "handshake.json prologue_xx_bare_address.prologue"
	xxInit := *p1
	xxInit.Access, xxInit.KnownCardSeq = nil, 0
	xx.p1 = xxInit.Marshal()
	xx.p1Src = "payloads.HsInit without access (field 4) and known_card_seq (field 5): P1 in XX must not carry access (handshake.md §4.2)"
	r, err := ParseHsResp(hsResp)
	if err != nil {
		t.Fatal(err)
	}
	r.Card = public
	xx.p2 = r.Marshal()
	xx.p2Src = "payloads.HsResp with card (field 4) = node-card.json public.envelope: the card is required in XX (handshake.md §7)"
	xx.p3, xx.p3Src = hsFinish, xk.p3Src
	xx.checks = []string{"same keys and KEM inputs as inputs_xk (handshake.json gives no separate XX inputs)"}

	f := tFile{
		Description: "Full XKhfs and XXhfs handshake transcripts (handshake.md Decision D15): every message's bytes, h/ck/k after every token, and the final transport keys. Produced by internal/v2/handshake (go test -run TestTranscriptVectors -update); to be confirmed byte for byte by an independent implementation before L1 is final.",
		Spec:        "handshake.md §2, §4, §5, §9",
		Conventions: []string{
			"hex is lowercase; every input is copied from handshake.json, node-card.json and link.json (sources named per field)",
			"steps: the state after each token; 'k' is the cipher key ('' when none) and 'n' the next nonce",
			"ML-KEM-768: KEM_GENERATE = crypto/mlkem NewDecapsulationKey768(d ‖ z); KEM_ENCAP with fixed m = FIPS 203 ML-KEM.Encaps_internal(ek, m) (Go crypto/mlkem/mlkemtest.Encapsulate768)",
			"h_before_payload is h right before EncryptAndHash(payload); for message 1 of XK it is h1 of the access proof (handshake.md §5)",
			"final.session_secret = Noise HKDF(ck, \"echofog/hs2/session-secret\").o1; final.sid = HKDF-SHA256(session_secret, salt = h, info = \"echofog/session2/id\", 16)",
		},
	}
	for _, in := range []transcriptInputs{xk, xkA, xx} {
		f.Transcripts = append(f.Transcripts, buildTranscript(t, in))
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestTranscriptVectors(t *testing.T) {
	got := buildTranscriptFile(t)
	path := filepath.Join(vectorDir, transcriptFile)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
		t.Fatal("handshake-transcript.json differs from the implementation (run with -update after review)")
	}
}
