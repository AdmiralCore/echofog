package main

// Noise XKhfs / XXhfs (handshake.md §2, §4) for the transcript vectors. Both roles run in lock step
// with separate symmetric states; every step of the writer must equal the reader's, so the code
// checks itself. Its output was cross-checked byte for byte against the two step-2 implementations
// (see the -crosscheck flag).

import (
	"bytes"
	"crypto/mlkem"
	"crypto/mlkem/mlkemtest"
	"encoding/json"
	"fmt"
	"os"
)

type symState struct {
	h, ck, k []byte
	n        uint64
}

func (s *symState) mixHash(d []byte) { s.h = b2s(s.h, d) }

func (s *symState) mixKey(ikm []byte) {
	s.ck, s.k = noiseHKDF(s.ck, ikm)
	s.n = 0
}

func (s *symState) encHash(p []byte) []byte {
	if s.k == nil {
		s.mixHash(p)
		return append([]byte(nil), p...)
	}
	c := chachaSeal(s.k, counterNonce(s.n), p, s.h)
	s.n++
	s.mixHash(c)
	return c
}

func (s *symState) decHash(c []byte) []byte {
	if s.k == nil {
		s.mixHash(c)
		return append([]byte(nil), c...)
	}
	p := chachaOpen(s.k, counterNonce(s.n), c, s.h)
	s.n++
	s.mixHash(c)
	return p
}

func (s *symState) step(tok string) O {
	k := ""
	if s.k != nil {
		k = hx(s.k)
	}
	return O{{"token", tok}, {"h", hx(s.h)}, {"ck", hx(s.ck)}, {"k", k}, {"n", s.n}}
}

type hsInputs struct {
	name, protocolName      string
	xk                      bool
	prologue                []byte
	is, rs, ie, re, seed, m []byte // private keys and KEM inputs
	p1, p2, p3              []byte
	p1Func                  func(h1 []byte) []byte
	sources                 map[string]string
	checks                  []string
}

type hsResult struct {
	o         O
	h1, h, ck []byte
	p1, msg1  []byte
}

func eqState(a, b *symState, what string) {
	check(bytes.Equal(a.h, b.h) && bytes.Equal(a.ck, b.ck) && bytes.Equal(a.k, b.k) && a.n == b.n, "noise state "+what)
}

// runHandshake executes the pattern and returns the transcript in the layout of
// vectors/handshake-transcript.json.
func runHandshake(in hsInputs) hsResult {
	I := &symState{h: noiseInit(in.protocolName)}
	I.ck = I.h
	R := &symState{h: I.h, ck: I.ck}
	var initSteps []any
	initSteps = append(initSteps, I.step("init"))
	I.mixHash(in.prologue)
	R.mixHash(in.prologue)
	initSteps = append(initSteps, I.step("prologue"))
	rsPub := x25519Pub(in.rs)
	if in.xk {
		I.mixHash(rsPub)
		R.mixHash(rsPub)
		initSteps = append(initSteps, I.step("pre:s"))
	}
	eqState(I, R, "init")

	ePub, rePub, sPub := x25519Pub(in.ie), x25519Pub(in.re), x25519Pub(in.is)
	dk, err := mlkem.NewDecapsulationKey768(in.seed)
	must(err)
	ek := dk.EncapsulationKey().Bytes()
	ssKEM, ct, err := mlkemtest.Encapsulate768(dk.EncapsulationKey(), in.m)
	must(err)

	var res hsResult
	var msgs []any
	// Each token: writer side w, reader side r; the closures append to the message and log.
	type tok struct {
		name string
		w    func(msg []byte) []byte
		r    func(msg []byte) []byte // returns the rest
	}
	run := func(num int, dir string, w, r *symState, toks []tok, payload func([]byte) []byte) {
		var msg []byte
		var steps []any
		var names []string
		for _, t := range toks {
			msg = t.w(msg)
			steps = append(steps, w.step(t.name))
			names = append(names, t.name)
		}
		hBefore := append([]byte(nil), w.h...)
		pl := payload(hBefore)
		msg = append(msg, w.encHash(pl)...)
		steps = append(steps, w.step("payload"))
		rest := msg
		for _, t := range toks {
			rest = t.r(rest)
		}
		check(bytes.Equal(r.h, hBefore), "reader h before payload")
		got := r.decHash(rest)
		check(bytes.Equal(got, pl), "payload round trip")
		eqState(w, r, fmt.Sprintf("after message %d", num))
		msgs = append(msgs, O{{"message", num}, {"direction", dir}, {"tokens", names}, {"payload", hx(pl)},
			{"h_before_payload", hx(hBefore)}, {"len", len(msg)}, {"sha256", hx(sha(msg))}, {"bytes", hx(msg)}, {"steps", steps}})
		if num == 1 {
			res.h1 = hBefore
			res.p1 = pl
			res.msg1 = msg
		}
	}
	take := func(b []byte, n int) ([]byte, []byte) { return b[:n], b[n:] }
	encLen := func(s *symState, n int) int {
		if s.k != nil {
			return n + 16
		}
		return n
	}
	tE := func(w, r *symState, pub []byte) tok {
		return tok{"e", func(m []byte) []byte { w.mixHash(pub); return append(m, pub...) },
			func(m []byte) []byte { v, rest := take(m, 32); r.mixHash(v); return rest }}
	}
	tE1 := tok{"e1", func(m []byte) []byte { return append(m, I.encHash(ek)...) },
		func(m []byte) []byte {
			v, rest := take(m, encLen(R, 1184))
			check(bytes.Equal(R.decHash(v), ek), "e1")
			return rest
		}}
	tEKEM := tok{"ekem1", func(m []byte) []byte { m = append(m, R.encHash(ct)...); R.mixKey(ssKEM); return m },
		func(m []byte) []byte {
			v, rest := take(m, encLen(I, 1088))
			c := I.decHash(v)
			ss, err := dk.Decapsulate(c)
			must(err)
			I.mixKey(ss)
			return rest
		}}
	tDH := func(name string, w, r *symState, wsk, rpub, rsk, wpub []byte) tok {
		return tok{name, func(m []byte) []byte { w.mixKey(x25519(wsk, rpub)); return m },
			func(m []byte) []byte { r.mixKey(x25519(rsk, wpub)); return m }}
	}
	tS := func(w, r *symState, pub []byte) tok {
		return tok{"s", func(m []byte) []byte { return append(m, w.encHash(pub)...) },
			func(m []byte) []byte {
				v, rest := take(m, encLen(r, 32))
				check(bytes.Equal(r.decHash(v), pub), "s")
				return rest
			}}
	}
	p1 := func(h []byte) []byte {
		if in.p1Func != nil {
			return in.p1Func(h)
		}
		return in.p1
	}
	if in.xk {
		run(1, "->", I, R, []tok{tE(I, R, ePub), tE1, tDH("es", I, R, in.ie, rsPub, in.rs, ePub)}, p1)
		run(2, "<-", R, I, []tok{tE(R, I, rePub), tDH("ee", R, I, in.re, ePub, in.ie, rePub), tEKEM}, func([]byte) []byte { return in.p2 })
		run(3, "->", I, R, []tok{tS(I, R, sPub), tDH("se", I, R, in.is, rePub, in.re, sPub)}, func([]byte) []byte { return in.p3 })
	} else {
		run(1, "->", I, R, []tok{tE(I, R, ePub), tE1}, p1)
		run(2, "<-", R, I, []tok{tE(R, I, rePub), tDH("ee", R, I, in.re, ePub, in.ie, rePub), tEKEM, tS(R, I, rsPub),
			tDH("es", R, I, in.rs, ePub, in.ie, rsPub)}, func([]byte) []byte { return in.p2 })
		run(3, "->", I, R, []tok{tS(I, R, sPub), tDH("se", I, R, in.is, rePub, in.re, sPub)}, func([]byte) []byte { return in.p3 })
	}
	kI2R, kR2I := noiseHKDF(I.ck, nil)
	sess, _ := noiseHKDF(I.ck, []byte("echofog/hs2/session-secret"))
	sid := hkdfSHA(sess, I.h, []byte("echofog/session2/id"), 16)
	// A wire.md §8.4 sealed record: n = 0, class 1 (ordered record), path 0, pipe 0.
	pt := frame(0x18, 0, cat(qv(4), []byte{0}, []byte("hello over a sealed path")))
	aad := cat(sid, u64(0), []byte{1}, qv(0), qv(0))
	sealed := cat(u64(0), chachaSeal(kI2R, counterNonce(0), pt, aad))
	record := cat(u16(uint16(1+len(sealed))), []byte{1}, sealed)
	res.h, res.ck = I.h, I.ck

	pattern := []string{"-> e, e1", "<- e, ee, ekem1, s, es", "-> s, se"}
	suite := 2
	if in.xk {
		pattern = []string{"<- s", "...", "-> e, e1, es", "<- e, ee, ekem1", "-> s, se"}
		suite = 1
	}
	src := func(k string) string { return in.sources[k] }
	res.o = O{
		{"name", in.name}, {"suite_id", suite}, {"protocol_name", in.protocolName}, {"pattern", pattern},
		{"inputs", O{
			{"prologue", hx(in.prologue)}, {"prologue_source", src("prologue")},
			{"initiator_s_sk", hx(in.is)}, {"initiator_s_pub", hx(sPub)},
			{"responder_s_sk", hx(in.rs)}, {"responder_s_pub", hx(rsPub)},
			{"initiator_e_sk", hx(in.ie)}, {"initiator_e_pub", hx(ePub)},
			{"responder_e_sk", hx(in.re)}, {"responder_e_pub", hx(rePub)},
			{"initiator_e1_seed_dz", hx(in.seed)}, {"initiator_e1_ek_sha256", hx(sha(ek))},
			{"encaps_randomness_m", hx(in.m)}, {"ekem1_ct_sha256", hx(sha(ct))}, {"ekem1_ss", hx(ssKEM)},
			{"p1", hx(res.p1)}, {"p1_source", src("p1")},
			{"p2", hx(in.p2)}, {"p2_source", src("p2")},
			{"p3", hx(in.p3)}, {"p3_source", src("p3")},
		}},
		{"initialize", initSteps},
		{"messages", msgs},
		{"final", O{
			{"h", hx(I.h)}, {"ck", hx(I.ck)}, {"k_i2r", hx(kI2R)}, {"k_r2i", hx(kR2I)},
			{"session_secret", hx(sess)}, {"sid", hx(sid)},
			{"sealed_record_example", O{{"plaintext", hx(pt)}, {"n", 0}, {"aad", hx(aad)}, {"sealed", hx(sealed)}, {"record", hx(record)},
				{"note", "wire.md §8.4: plaintext = a MUX_STREAM frame (stream 4); aad = sid ‖ u64(n) ‖ class 1 ‖ varint path 0 ‖ varint pipe 0; sealed = u64(n) ‖ ChaCha20-Poly1305(k_i2r, nonce(n), aad, plaintext); record = u16(len) ‖ kind 1 ‖ sealed"}}},
		}},
		{"checks", in.checks},
	}
	return res
}

// crossCheck recomputes every transcript of a handshake-transcript.json written by another
// implementation from its own inputs and compares, as Decision D15 requires, every initialisation
// step, every message's bytes and every step's h/ck/k/n, and the final h, ck, k_i2r, k_r2i,
// session_secret and sid.
func crossCheck(path string) {
	raw, err := os.ReadFile(path)
	must(err)
	var f struct {
		Transcripts []map[string]any
	}
	must(json.Unmarshal(raw, &f))
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	for _, t := range f.Transcripts {
		name, pn := str(t, "name"), str(t, "protocol_name")
		in := t["inputs"].(map[string]any)
		r := runHandshake(hsInputs{
			name: name, protocolName: pn, xk: bytes.Contains([]byte(pn), []byte("XK")),
			prologue: unhex(str(in, "prologue")), is: unhex(str(in, "initiator_s_sk")), rs: unhex(str(in, "responder_s_sk")),
			ie: unhex(str(in, "initiator_e_sk")), re: unhex(str(in, "responder_e_sk")), seed: unhex(str(in, "initiator_e1_seed_dz")),
			m: unhex(str(in, "encaps_randomness_m")), p1: unhex(str(in, "p1")), p2: unhex(str(in, "p2")), p3: unhex(str(in, "p3")),
		})
		mineRaw, err := marshal(r.o)
		must(err)
		var mine map[string]any
		must(json.Unmarshal(mineRaw, &mine))
		same := func(a, b any) bool {
			x, _ := json.Marshal(a)
			y, _ := json.Marshal(b)
			return bytes.Equal(x, y)
		}
		check(same(t["initialize"], mine["initialize"]), "crosscheck "+name+" initialisation steps")
		theirs, ours := t["messages"].([]any), mine["messages"].([]any)
		check(len(theirs) == len(ours), "crosscheck "+name+" message count")
		for i := range theirs {
			a, b := theirs[i].(map[string]any), ours[i].(map[string]any)
			check(a["bytes"] == b["bytes"], fmt.Sprintf("crosscheck %s message %d bytes", name, i+1))
			check(same(a["steps"], b["steps"]), fmt.Sprintf("crosscheck %s message %d steps", name, i+1))
		}
		tf, mf := t["final"].(map[string]any), mine["final"].(map[string]any)
		for _, k := range []string{"h", "ck", "k_i2r", "k_r2i", "session_secret", "sid"} {
			check(tf[k] == mf[k], "crosscheck "+name+" final "+k)
		}
		fmt.Println("crosscheck ok:", name)
	}
}
