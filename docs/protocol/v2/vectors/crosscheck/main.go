// Command crosscheck is a clean-room second implementation of the EchoFog v2 L1 handshake
// (handshake.md, Noise XKhfs / XXhfs), written only from the specification text and the existing
// vectors, to cross-check the main implementation (handshake.md Decision D15).
//
// It is a reference handshake, not product code. Run from this directory:
//
//	GOWORK=off go run . -out .
//
// and it writes transcript-cleanroom.json. NOTES.md lists every reading of the spec it had to choose.
package main

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// ---- scenario construction ---------------------------------------------------------------------

type seedRef struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type scenario struct {
	ID    string
	XX    bool
	Notes []string
	Seeds []seedRef
	In    Inputs
	Rid   []byte
	CB    []byte
}

func useSeed(sc *scenario, name string) []byte {
	v := seed(name)
	sc.Seeds = append(sc.Seeds, seedRef{name, hexs(v)})
	return v
}

var (
	helloXK = join([]byte{2}, u16be(1), u16be(2), u16be(1), []byte{0x02}) // handshake.json prologue_xk
	helloXX = join([]byte{1}, u16be(2), u16be(2), []byte{0x00})           // prologue_xx_bare_address
)

// keyPrefix names the seeds: "hs/xk/" (the names handshake.json defines) or "hs/xx/" (our choice).
func baseScenario(id string, xx bool, keyPrefix string) scenario {
	sc := scenario{ID: id, XX: xx}
	sc.CB = useSeed(&sc, "hs/cb")
	respS := useSeed(&sc, "card/hs_sk")
	devSeed := useSeed(&sc, "card/device_seed")
	initE := useSeed(&sc, keyPrefix+"initiator_e")
	initS := useSeed(&sc, keyPrefix+"initiator_s")
	respE := useSeed(&sc, keyPrefix+"responder_e")
	d := useSeed(&sc, keyPrefix+"initiator_e1_d")
	z := useSeed(&sc, keyPrefix+"initiator_e1_z")
	m := useSeed(&sc, keyPrefix+"encaps_m")

	name, hello, rid := nameXK, helloXK, deviceID(devSeed)
	if xx {
		name, hello, rid = nameXX, helloXX, make([]byte, 32)
	}
	sc.Rid = rid
	sc.In = Inputs{
		Name: name, Hello: hello, Prologue: prologue(sc.CB, rid, hello),
		RespS: respS, InitS: initS, InitE: initE, RespE: respE,
		KemSeed: join(d, z), EncapsM: m,
	}
	return sc
}

// checkCardInP checks that the card in a payload (field cardField) has hs_key = 0x02 ‖ rs.
func checkCardInP(cardField int) func(p, rs []byte) error {
	return func(p, rs []byte) error {
		ld, _, err := pbFields(p)
		if err != nil {
			return err
		}
		card, ok := ld[cardField]
		if !ok {
			return errors.New("no card")
		}
		hk, err := cardHSKey(card)
		if err != nil {
			return err
		}
		if !bytes.Equal(hk, join([]byte{0x02}, rs)) {
			return fmt.Errorf("card hs_key %x != 02‖rs %x", hk, rs)
		}
		return nil
	}
}

func scenarios() []scenario {
	S := accessSecret()
	pid := passID(S)
	fixedH1 := seed("hs/h1")
	peer := peerCardEnvelope()
	public := publicCardEnvelope()
	knownSeq := uint64(fixedTime - 86400)

	// 1. XK, payloads literally as handshake.json deferred_transcripts.payloads says.
	xk := baseScenario("xk", false, "hs/xk/")
	xk.Seeds = append(xk.Seeds, seedRef{"hs/h1", hexs(fixedH1)}, seedRef{"link/access_secret (first 16 bytes)", hexs(S)})
	literalP1 := hsInit(accessMsg(pid, accessProof(S, fixedH1)), knownSeq)
	xk.In.P1 = func([]byte) []byte { return literalP1 }
	xk.In.P2 = hsResp(nil)
	xk.In.P3 = hsFinish(peer)
	xk.In.CheckP3 = checkCardInP(1)
	xk.Notes = []string{
		"P1, P2, P3 are byte-for-byte payloads.HsInit, payloads.HsResp, payloads.HsFinish of handshake.json (deferred_transcripts.payloads).",
		"P1.access.proof is therefore computed over the fixed seed hs/h1, NOT over the real h1 of this run; a real hidden responder would reject it (see xk_live_access).",
		"P2 carries no card although known_card_seq (ts-86400) is older than the responder's peer card seq; P3's card is the responder's own peer card (format illustration), so its hs_key does not match the initiator's s (card_checks).",
	}

	// 2. XK with the access proof computed over the run's real h1 (§5), checked by the responder.
	live := baseScenario("xk_live_access", false, "hs/xk/")
	live.Seeds = append(live.Seeds, seedRef{"link/access_secret (first 16 bytes)", hexs(S)})
	live.In.P1 = func(h1 []byte) []byte { return hsInit(accessMsg(pid, accessProof(S, h1)), knownSeq) }
	live.In.CheckP1 = func(h1, p1 []byte) error {
		ld, _, err := pbFields(p1)
		if err != nil {
			return err
		}
		acc, _, err := pbFields(ld[4])
		if err != nil {
			return err
		}
		if !bytes.Equal(acc[1], pid) || !bytes.Equal(acc[2], accessProof(S, h1)) {
			return errors.New("access proof invalid")
		}
		return nil
	}
	live.In.P2 = hsResp(nil)
	live.In.P3 = hsFinish(peer)
	live.In.CheckP3 = checkCardInP(1)
	live.Notes = []string{
		"Same inputs as xk; only P1.access.proof differs: HMAC-SHA256(S, \"echofog/hs2/access-proof\" ‖ h1) with h1 = h after step 1.3 of this run. The responder verifies it.",
	}

	// 3. XX with seeds named hs/xx/* (handshake.json gives no XX inputs).
	xxP1 := hsInit(nil, 0) // §4.2: no access, no early; known_card_seq is XK-only, so absent
	xx := baseScenario("xx", true, "hs/xx/")
	xx.In.P1 = func([]byte) []byte { return xxP1 }
	xx.In.P2 = hsResp(public)
	xx.In.P3 = hsFinish(peer)
	xx.In.CheckP2 = checkCardInP(4)
	xx.In.CheckP3 = checkCardInP(1)
	xx.Notes = []string{
		"handshake.json defines no XX transcript inputs. Seeds are named like the XK ones with the prefix hs/xx/ instead of hs/xk/ (our choice). Responder static = node-card.json hs_sk as in XK.",
		"Prologue = handshake.json prologue_xx_bare_address (offered [0x0002], chosen 0x0002, flags 0, rid = 32 zero bytes).",
		"P1 = payloads.HsInit without access (field 4) and without known_card_seq (field 5): §4.2 forbids access in XX and known_card_seq is described as (XK).",
		"P2 = payloads.HsResp plus card (field 4) = node-card.json public card envelope (card is REQUIRED in XX; we chose the public scope for a bootstrap responder).",
		"P3 = payloads.HsFinish (responder's peer card, format illustration; a real XX initiator to a bare address would send a throwaway card).",
	}

	// 4. XX again, but reusing the hs/xk/* seeds, in case the other implementation did that.
	xxk := baseScenario("xx_with_xk_seeds", true, "hs/xk/")
	xxk.In.P1 = xx.In.P1
	xxk.In.P2, xxk.In.P3 = xx.In.P2, xx.In.P3
	xxk.In.CheckP2, xxk.In.CheckP3 = xx.In.CheckP2, xx.In.CheckP3
	xxk.Notes = []string{"As xx, but every ephemeral, static, ML-KEM seed and encapsulation randomness reuses the hs/xk/* seeds of handshake.json deferred_transcripts.inputs_xk."}

	return []scenario{xk, live, xx, xxk}
}

// ---- session derivations (§9) ----------------------------------------------------------------

func sessionSecret(ck []byte) []byte {
	o1, _ := noiseHKDF2(ck, []byte("echofog/hs2/session-secret"))
	return o1
}

func sessionID(sessSecret, h []byte) []byte {
	out, err := hkdf.Key(sha256.New, sessSecret, h, "echofog/session2/id", 16)
	if err != nil {
		panic(err)
	}
	return out
}

// ---- JSON output ----------------------------------------------------------------------------

type msgOut struct {
	N          int    `json:"n"`
	Dir        string `json:"dir"`
	Len        int    `json:"len"`
	Noise      string `json:"noise_message"`
	HSMsgFrame string `json:"hs_msg_frame"`
}

type payloadOut struct {
	Len    int    `json:"len"`
	SHA256 string `json:"sha256"`
	Hex    string `json:"hex"`
}

type scenarioOut struct {
	ID           string                `json:"id"`
	ProtocolName string                `json:"protocol_name"`
	Notes        []string              `json:"choices"`
	Seeds        []seedRef             `json:"seeds"`
	Keys         map[string]string     `json:"keys"`
	HSHello      string                `json:"hs_hello"`
	HSHelloFrame string                `json:"hs_hello_frame"`
	CB           string                `json:"cb"`
	Rid          string                `json:"rid"`
	Prologue     string                `json:"prologue"`
	Payloads     map[string]payloadOut `json:"payloads"`
	Steps        []Step                `json:"steps"`
	Messages     []msgOut              `json:"messages"`
	FirstFlight  string                `json:"first_flight (HS_HELLO frame ‖ HS_MSG frame 1)"`
	Derived      map[string]string     `json:"derived"`
	CardChecks   map[string]bool       `json:"card_checks"`
	Final        map[string]string     `json:"final"`
}

func runScenario(sc scenario) (scenarioOut, error) {
	ini, resp, err := run(sc.XX, sc.In)
	if err != nil {
		return scenarioOut{}, fmt.Errorf("%s: %w", sc.ID, err)
	}
	cardChecks := map[string]bool{}
	for k, v := range ini.CardOK {
		cardChecks[k] = v
	}
	for k, v := range resp.CardOK {
		cardChecks[k] = v
	}

	in := sc.In
	keys := map[string]string{
		"initiator_e_pub":   hexs(x25519Public(in.InitE)),
		"initiator_s_pub":   hexs(x25519Public(in.InitS)),
		"responder_e_pub":   hexs(x25519Public(in.RespE)),
		"responder_s_pub":   hexs(x25519Public(in.RespS)),
		"initiator_e1_seed": hexs(in.KemSeed),
		"encaps_m":          hexs(in.EncapsM),
	}
	dirs := []string{"initiator->responder", "responder->initiator", "initiator->responder"}
	var msgs []msgOut
	for i, m := range ini.Msgs {
		msgs = append(msgs, msgOut{i + 1, dirs[i], len(m), hexs(m), hexs(streamFrame(0x03, m))})
	}
	pl := func(b []byte) payloadOut { return payloadOut{len(b), hexs(sha(b)), hexs(b)} }
	ss := sessionSecret(ini.CK)
	return scenarioOut{
		ID: sc.ID, ProtocolName: in.Name, Notes: sc.Notes, Seeds: sc.Seeds, Keys: keys,
		HSHello: hexs(in.Hello), HSHelloFrame: hexs(streamFrame(0x02, in.Hello)),
		CB: hexs(sc.CB), Rid: hexs(sc.Rid), Prologue: hexs(in.Prologue),
		Payloads: map[string]payloadOut{"P1": pl(ini.P1), "P2": pl(ini.P2), "P3": pl(ini.P3)},
		Steps:    ini.Steps, Messages: msgs,
		FirstFlight: hexs(join(streamFrame(0x02, in.Hello), streamFrame(0x03, ini.Msgs[0]))),
		Derived: map[string]string{
			"h1 (h before EncryptAndHash(P1))": hexs(ini.Extra["h1"]),
			"e1_ek_sha256":                     hexs(ini.Extra["e1_ek_sha256"]),
			"ekem1_ct_sha256":                  hexs(ini.Extra["ekem1_ct_sha256"]),
			"ekem1_ss":                         hexs(ini.Extra["ekem1_ss"]),
		},
		CardChecks: cardChecks,
		Final: map[string]string{
			"h":              hexs(ini.H),
			"ck":             hexs(ini.CK),
			"k_i2r":          hexs(ini.KI2R),
			"k_r2i":          hexs(ini.KR2I),
			"session_secret": hexs(ss),
			"sid":            hexs(sessionID(ss, ini.H)),
		},
	}, nil
}

type fileOut struct {
	Description string        `json:"description"`
	Spec        string        `json:"spec"`
	Conventions []string      `json:"conventions"`
	Transcripts []scenarioOut `json:"transcripts"`
}

func main() {
	out := flag.String("out", ".", "directory to write transcript-cleanroom.json to")
	vd := flag.String("vectors", "..", "directory of the existing vector files")
	flag.Parse()
	vectorDir = *vd

	f := fileOut{
		Description: "Clean-room XKhfs/XXhfs transcripts (handshake.md D15), independent of internal/v2/handshake. See NOTES.md for every reading of the spec chosen here.",
		Spec:        "handshake.md §2–§4, §9",
		Conventions: []string{
			"Seeds: SHA-256(\"echofog v2 test vector seed: \" ‖ name), as vectors/README.md.",
			"X25519 secrets are the raw 32-byte seed (clamped by X25519 itself). ML-KEM-768 key from the 64-byte seed d ‖ z (crypto/mlkem NewDecapsulationKey768), encapsulation with fixed m (crypto/mlkem/mlkemtest Encapsulate768).",
			"steps: h and ck after each token; k is the current cipher key (absent = no key) and n the nonce that the next EncryptAndHash would use.",
			"messages[].noise_message is the Noise message; hs_msg_frame wraps it as an HS_MSG stream frame (type 0x03, flags 0, QUIC-varint length).",
			"final: k_i2r, k_r2i = Split() = Noise HKDF(ck, empty, 2); session_secret = Noise HKDF(ck, \"echofog/hs2/session-secret\", 2).o1; sid = HKDF-SHA256(session_secret, salt = h, info = \"echofog/session2/id\", 16).",
		},
	}
	for _, sc := range scenarios() {
		so, err := runScenario(sc)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		f.Transcripts = append(f.Transcripts, so)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		panic(err)
	}
	b = append(b, '\n')
	p := filepath.Join(*out, "transcript-cleanroom.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		panic(err)
	}
	fmt.Println("wrote", p)
}
