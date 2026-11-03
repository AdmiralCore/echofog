package main

import (
	"bytes"
	"os"
	"testing"
)

func eqHex(t *testing.T, what string, got []byte, want string) {
	t.Helper()
	if hexs(got) != want {
		t.Errorf("%s:\n got  %s\n want %s", what, hexs(got), want)
	}
}

// ---- handshake.json -------------------------------------------------------------------------

func TestInitializeSymmetric(t *testing.T) {
	hs := loadJSON("handshake.json")
	for i, e := range hs["initialize_symmetric"].([]any) {
		m := e.(map[string]any)
		name := m["protocol_name"].(string)
		if name != []string{nameXK, nameXX}[i] {
			t.Fatalf("protocol name %d: %s", i, name)
		}
		h := initialH(name)
		eqHex(t, name+" h0", h, m["h0"].(string))
		eqHex(t, name+" ck0", h, m["ck0"].(string))
	}
}

func TestPrologues(t *testing.T) {
	hs := loadJSON("handshake.json")
	cb := seed("hs/cb")
	rid := deviceID(seed("card/device_seed"))
	eqHex(t, "cb", cb, jget(hs, "prologue_xk", "cb"))
	eqHex(t, "rid", rid, jget(hs, "prologue_xk", "rid (node-card.json device_id)"))
	eqHex(t, "rid vs node-card", rid, jget(loadJSON("node-card.json"), "device_id (SHA-256 of the device akey; the card signer)"))
	eqHex(t, "hello xk", helloXK, jget(hs, "prologue_xk", "hs_hello"))
	pXK := prologue(cb, rid, helloXK)
	eqHex(t, "prologue xk", pXK, jget(hs, "prologue_xk", "prologue"))
	h := mixHash(initialH(nameXK), pXK)
	eqHex(t, "h after prologue xk", h, jget(hs, "prologue_xk", "h_after_prologue"))
	rs := x25519Public(seed("card/hs_sk"))
	eqHex(t, "rs", rs, jget(hs, "prologue_xk", "responder_static_rs"))
	eqHex(t, "h after rs", mixHash(h, rs), jget(hs, "prologue_xk", "h_after_premessage_rs"))

	eqHex(t, "hello xx", helloXX, jget(hs, "prologue_xx_bare_address", "hs_hello"))
	pXX := prologue(cb, make([]byte, 32), helloXX)
	eqHex(t, "prologue xx", pXX, jget(hs, "prologue_xx_bare_address", "prologue"))
	eqHex(t, "h after prologue xx", mixHash(initialH(nameXX), pXX), jget(hs, "prologue_xx_bare_address", "h_after_prologue"))
}

func TestAccess(t *testing.T) {
	hs := loadJSON("handshake.json")
	S := accessSecret()
	eqHex(t, "S", S, jget(hs, "access", "secret (link.json)"))
	eqHex(t, "h1", seed("hs/h1"), jget(hs, "access", "h1"))
	eqHex(t, "pass_id", passID(S), jget(hs, "access", "pass_id"))
	eqHex(t, "proof", accessProof(S, seed("hs/h1")), jget(hs, "access", "proof"))
}

func TestPayloadEncodings(t *testing.T) {
	hs := loadJSON("handshake.json")
	S := accessSecret()
	p1 := hsInit(accessMsg(passID(S), accessProof(S, seed("hs/h1"))), fixedTime-86400)
	eqHex(t, "HsInit", p1, jget(hs, "payloads", "HsInit", "encoded"))
	eqHex(t, "HsResp", hsResp(nil), jget(hs, "payloads", "HsResp", "encoded"))
	p3 := hsFinish(peerCardEnvelope())
	eqHex(t, "HsFinish sha256", sha(p3), jget(hs, "payloads", "HsFinish", "encoded_sha256"))
	if want := hs["payloads"].(map[string]any)["HsFinish"].(map[string]any)["encoded_len"].(float64); len(p3) != int(want) {
		t.Errorf("HsFinish len %d, want %v", len(p3), want)
	}
}

func leadingZeroBits(b []byte) int {
	n := 0
	for _, x := range b {
		for i := 7; i >= 0; i-- {
			if x>>uint(i)&1 != 0 {
				return n
			}
			n++
		}
	}
	return n
}

func TestPuzzle(t *testing.T) {
	hs := loadJSON("handshake.json")
	cb := seed("hs/cb")
	R := seed("hs/cookie_secret")
	minute := uint64(fixedTime / 60)
	cookie := hmacSHA(R, []byte("echofog/hs2/cookie"), cb, u64be(minute))[:16]
	eqHex(t, "cookie", cookie, jget(hs, "puzzle", "cookie"))
	var nonce uint64
	var ph []byte
	for ; ; nonce++ {
		ph = sha([]byte("echofog/hs2/puzzle"), cookie, cb, u64be(nonce))
		if leadingZeroBits(ph) >= 12 {
			break
		}
	}
	if want := hs["puzzle"].(map[string]any)["nonce"].(float64); nonce != uint64(want) {
		t.Errorf("nonce %d want %v", nonce, want)
	}
	eqHex(t, "puzzle hash", ph, jget(hs, "puzzle", "hash"))
	eqHex(t, "HS_RETRY frame", streamFrame(0x04, join(cookie, []byte{12})), jget(hs, "puzzle", "HS_RETRY_frame"))
	hello := join([]byte{2}, u16be(1), u16be(2), u16be(1), []byte{0x03}, cookie, u64be(nonce))
	eqHex(t, "HS_HELLO with solution", hello, jget(hs, "puzzle", "HS_HELLO_with_solution_payload"))
}

func TestDeferredInputs(t *testing.T) {
	hs := loadJSON("handshake.json")
	get := func(k string) string { return jget(hs, "deferred_transcripts", "inputs_xk", k) }
	eqHex(t, "initiator_e_sk", seed("hs/xk/initiator_e"), get("initiator_e_sk"))
	eqHex(t, "initiator_e_pub", x25519Public(seed("hs/xk/initiator_e")), get("initiator_e_pub"))
	eqHex(t, "initiator_s_pub", x25519Public(seed("hs/xk/initiator_s")), get("initiator_s_pub"))
	eqHex(t, "responder_e_pub", x25519Public(seed("hs/xk/responder_e")), get("responder_e_pub"))
	dz := join(seed("hs/xk/initiator_e1_d"), seed("hs/xk/initiator_e1_z"))
	eqHex(t, "e1 seed", dz, get("initiator_e1_seed_dz"))
	dk, ek := kemGenerate(dz)
	eqHex(t, "ek sha256", sha(ek), get("initiator_e1_ek_sha256"))
	eqHex(t, "m", seed("hs/xk/encaps_m"), get("encaps_randomness_m"))
	ct, ss, err := kemEncap(ek, seed("hs/xk/encaps_m"))
	if err != nil {
		t.Fatal(err)
	}
	eqHex(t, "ct sha256", sha(ct), get("ekem1_ct_sha256"))
	eqHex(t, "ss", ss, get("ekem1_ss"))
	ss2, err := dk.Decapsulate(ct)
	if err != nil || !bytes.Equal(ss, ss2) {
		t.Fatal("decap mismatch")
	}
	eqHex(t, "responder hs_sk", seed("card/hs_sk"), jget(loadJSON("node-card.json"), "hs_sk"))
}

// ---- kdf.json -------------------------------------------------------------------------------

func TestSessionKDF(t *testing.T) {
	kd := loadJSON("kdf.json")
	ck := seed("session/ck")
	h := seed("session/h")
	eqHex(t, "ck", ck, jget(kd, "session", "ck"))
	eqHex(t, "h", h, jget(kd, "session", "h"))
	k1, k2 := noiseHKDF2(ck, nil)
	eqHex(t, "split k_i2r", k1, jget(kd, "session", "split_k_i2r (noise HKDF(ck, empty).o1)"))
	eqHex(t, "split k_r2i", k2, jget(kd, "session", "split_k_r2i (noise HKDF(ck, empty).o2)"))
	ss := sessionSecret(ck)
	eqHex(t, "session_secret", ss, jget(kd, "session", "session_secret (noise HKDF(ck, label).o1)"))
	eqHex(t, "sid", sessionID(ss, h), jget(kd, "session", "sid"))
	// sealed record: the transport key and nonce layout used with k_i2r (wire.md §8.4).
	pt := fromHex(jget(kd, "sealed_record", "plaintext"))
	sid := sessionID(ss, h)
	sealed := join(u64be(0), aeadSeal(k1, 0, join(sid, u64be(0)), pt))
	eqHex(t, "sealed record", sealed, jget(kd, "sealed_record", "sealed"))
}

// ---- round trips ----------------------------------------------------------------------------

func TestRoundTrips(t *testing.T) {
	for _, sc := range scenarios() {
		ini, resp, err := run(sc.XX, sc.In)
		if err != nil {
			t.Fatalf("%s: %v", sc.ID, err)
		}
		if !bytes.Equal(ini.KI2R, resp.KI2R) || bytes.Equal(ini.KI2R, ini.KR2I) {
			t.Fatalf("%s: keys", sc.ID)
		}
		// handshake.md §4.1 sizes without payload; XX sizes follow from §4.2.
		want := []int{1232, 1152, 64}
		if sc.XX {
			want = []int{32 + 1184, 32 + 1104 + 48 + 16, 64}
		}
		pls := [][]byte{ini.P1, ini.P2, ini.P3}
		for i, m := range ini.Msgs {
			if len(m)-len(pls[i]) != want[i] {
				t.Errorf("%s: message %d overhead %d, want %d", sc.ID, i+1, len(m)-len(pls[i]), want[i])
			}
		}
		// the XK pre-message h and XX prologue h equal handshake.json
		if !sc.XX && ini.Steps[2].H != "b3581ff5d9eb76792fa78daea88e87ec515cca5e7958c264a7c366c87fb936ab" {
			t.Errorf("%s: pre-message h", sc.ID)
		}
		if sc.ID == "xk" || sc.ID == "xx_with_xk_seeds" {
			if hexs(ini.Extra["ekem1_ss"]) != "218ea1ad2ba9ace442619182df4932a0d90ca7f903ddc81056e70a9d247abb63" {
				t.Errorf("%s: ekem1 ss", sc.ID)
			}
		}
	}
}

// runMITM runs a handshake with a forwarder that may change messages.
func runMITM(xx bool, in Inputs, mutate func(n int, m []byte) []byte) (error, error) {
	a, b := newPipe(), newPipe()
	go func() {
		defer close(b.toResp)
		defer close(a.toInit)
		m1, ok := <-a.toResp
		if !ok {
			return
		}
		b.toResp <- mutate(1, append([]byte(nil), m1...))
		m2, ok := <-b.toInit
		if !ok {
			return
		}
		a.toInit <- mutate(2, append([]byte(nil), m2...))
		m3, ok := <-a.toResp
		if !ok {
			return
		}
		b.toResp <- mutate(3, append([]byte(nil), m3...))
	}()
	errc := make(chan error, 1)
	go func() {
		var err error
		if xx {
			_, err = xxResponder(in, b)
		} else {
			_, err = xkResponder(in, b)
		}
		errc <- err
	}()
	var ierr error
	if xx {
		_, ierr = xxInitiator(in, a)
	} else {
		_, ierr = xkInitiator(in, x25519Public(in.RespS), a)
	}
	return ierr, <-errc
}

func TestTamperFails(t *testing.T) {
	for _, sc := range scenarios() {
		if sc.ID == "xx_with_xk_seeds" {
			continue
		}
		for msg := 1; msg <= 3; msg++ {
			for _, pos := range []int{0, 40, 1300, -1} {
				ierr, rerr := runMITM(sc.XX, sc.In, func(n int, m []byte) []byte {
					if n == msg {
						p := pos
						if p < 0 || p >= len(m) {
							p = len(m) - 1
						}
						m[p] ^= 0x01
					}
					return m
				})
				if ierr == nil && rerr == nil {
					// XX message 1 is unencrypted: a flipped bit still yields a handshake whose
					// transcripts differ, so it must fail later at the latest.
					t.Errorf("%s: flip in message %d at %d accepted by both", sc.ID, msg, pos)
				}
			}
		}
	}
}

func TestPrologueMismatchFails(t *testing.T) {
	sc := scenarios()[0]
	bad := sc.In
	ierr, rerr := func() (error, error) {
		a := newPipe()
		errc := make(chan error, 1)
		r := bad
		r.Prologue = append(append([]byte(nil), bad.Prologue...), 0) // responder sees another prologue
		go func() { _, err := xkResponder(r, a); errc <- err }()
		_, err := xkInitiator(bad, x25519Public(bad.RespS), a)
		return err, <-errc
	}()
	if rerr == nil {
		t.Fatalf("prologue mismatch accepted (initiator err %v)", ierr)
	}
}

func TestZeroDHRejected(t *testing.T) {
	if _, err := dh(seed("hs/xk/initiator_e"), make([]byte, 32)); err == nil {
		t.Fatal("all-zero DH accepted")
	}
	// a small-order point (order 8) also gives the all-zero output
	low := fromHex("e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800")
	if _, err := dh(seed("hs/xk/initiator_e"), low); err == nil {
		t.Fatal("small-order DH accepted")
	}
}

func TestBadEncapsulationKeyRejected(t *testing.T) {
	_, ek := kemGenerate(join(seed("hs/xk/initiator_e1_d"), seed("hs/xk/initiator_e1_z")))
	bad := append([]byte(nil), ek...)
	bad[0], bad[1] = 0xff, 0x0f // first 12-bit coefficient = 0xfff ≥ q = 3329
	if _, _, err := kemEncap(bad, seed("hs/xk/encaps_m")); err == nil {
		t.Fatal("non-reduced ML-KEM encapsulation key accepted")
	}
}

// The committed transcript file must equal what this code produces.
func TestTranscriptFileCurrent(t *testing.T) {
	dir := t.TempDir()
	os.Args = []string{"crosscheck", "-out", dir}
	main()
	got, err := os.ReadFile(dir + "/transcript-cleanroom.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("transcript-cleanroom.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
		t.Fatal("transcript-cleanroom.json is stale: run GOWORK=off go run . -out .")
	}
}
