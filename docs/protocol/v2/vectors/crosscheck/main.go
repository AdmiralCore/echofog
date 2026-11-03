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
	helloXK       = hsHello([]uint16{1, 2}, 0x02)                    // prologue_xk
	helloXKHidden = hsHello(insertGrease([]uint16{1}, 0x0a0a), 0x02) // prologue_xk_hidden
	helloXX       = hsHello([]uint16{2}, 0x00)                       // prologue_xx_bare_address
)

// baseScenario uses the transcript_inputs of handshake.json (the hs/xk/* seeds) for all patterns.
func baseScenario(id string, xx bool, hello []byte) scenario {
	sc := scenario{ID: id, XX: xx}
	sc.CB = useSeed(&sc, "hs/cb")
	respS := useSeed(&sc, "card/hs_sk")
	devSeed := useSeed(&sc, "card/device_seed")
	initE := useSeed(&sc, "hs/xk/initiator_e")
	initS := useSeed(&sc, "hs/xk/initiator_s")
	respE := useSeed(&sc, "hs/xk/responder_e")
	d := useSeed(&sc, "hs/xk/initiator_e1_d")
	z := useSeed(&sc, "hs/xk/initiator_e1_z")
	m := useSeed(&sc, "hs/xk/encaps_m")

	name, rid := nameXK, deviceID(devSeed)
	if xx {
		name, rid = nameXX, make([]byte, 32)
	}
	sc.Rid = rid
	sc.In = Inputs{
		Name: name, Hello: hello, Prologue: prologue(sc.CB, rid, hello),
		RespS: respS, InitS: initS, InitE: initE, RespE: respE,
		KemSeed: join(d, z), EncapsM: m,
	}
	return sc
}

// checkCard checks a card in a payload (field cardField): hs_key = 0x02 ‖ rs and, when
// expectSigner is set, the signer's device ID.
func checkCard(cardField int, expectDevice []byte) func(p, rs []byte) error {
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
		if expectDevice != nil && !bytes.Equal(sha(cardSigner(card)), expectDevice) {
			return errors.New("card signer is not the expected device")
		}
		return nil
	}
}

func scenarios() []scenario {
	S := accessSecret()
	pid := passID(S)
	pubSeq := cardSeq(cardEnvelope("public"))
	invSeq := cardSeq(cardEnvelope("invite"))
	initiatorCard := cardEnvelope("initiator")
	throwawayCard := cardEnvelope("initiator_throwaway")
	initDev := fromHex(jget(loadJSON("node-card.json"), "initiator", "device_id"))
	throwDev := fromHex(jget(loadJSON("node-card.json"), "initiator_throwaway", "device_id"))

	// 1. XKhfs to the public card.
	xk := baseScenario("XKhfs", false, helloXK)
	p1xk := hsInit(nil, pubSeq)
	xk.In.P1 = func([]byte) []byte { return p1xk }
	xk.In.P2 = hsResp(p2Card(false, pubSeq))
	xk.In.P3 = hsFinish(initiatorCard)
	xk.In.CheckP3 = checkCard(1, initDev)
	xk.Notes = []string{"prologue_xk; P1 = HsInit_xk (known_card_seq = public card seq); P2 = HsResp (no card: same seq); P3 = HsFinish (initiator card)."}

	// 2. XKhfs to a hidden node's invitation card, access proof over this run's h1.
	hid := baseScenario("XKhfs-access-proof", false, helloXKHidden)
	hid.Seeds = append(hid.Seeds, seedRef{"link/access_secret (first 16 bytes)", hexs(S)})
	hid.In.P1 = func(h1 []byte) []byte { return hsInit(accessMsg(pid, accessProof(S, h1)), invSeq) }
	hid.In.CheckP1 = func(h1, p1 []byte) error {
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
	hid.In.P2 = hsResp(p2Card(false, invSeq))
	hid.In.P3 = hsFinish(initiatorCard)
	hid.In.CheckP3 = checkCard(1, initDev)
	hid.Notes = []string{"prologue_xk_hidden (offered [0x0001, 0x0a0a]); P1 = HsInit_xk_hidden (access proof over the real h1, known_card_seq = invite card seq); P2 = HsResp (no card for an invitation card); P3 = HsFinish."}

	// 3. XXhfs to a bare address, same keys.
	xx := baseScenario("XXhfs", true, helloXX)
	p1xx := hsInit(nil, 0)
	xx.In.P1 = func([]byte) []byte { return p1xx }
	xx.In.P2 = hsResp(p2Card(true, 0))
	xx.In.P3 = hsFinish(throwawayCard)
	xx.In.CheckP2 = checkCard(4, nil) // rid is zero: no expected device ID
	xx.In.CheckP3 = checkCard(1, throwDev)
	xx.Notes = []string{"prologue_xx_bare_address; P1 = HsInit_xx; P2 = HsResp_xx (public card); P3 = HsFinish_xx (throwaway card). Keys = transcript_inputs (the XK seeds)."}

	return []scenario{xk, hid, xx}
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
			"session_secret": hexs(ini.SessionSecret),
			"sid":            hexs(ini.SID),
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
