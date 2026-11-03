package main

import (
	"bytes"
	"errors"
	"fmt"
)

// Each pattern is two straight-line functions, one per role, that exchange Noise messages over
// a pipe. The token order is spelled out line by line from handshake.md §4.1 / §4.2.

const (
	nameXK = "EchoFog/2/Noise_XKhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s"
	nameXX = "EchoFog/2/Noise_XXhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s"

	lenPub   = 32
	lenEK    = 1184
	lenCT    = 1088
	tagLen   = 16
	maxNoise = 16384
)

// prologue is handshake.md §3.3.
func prologue(cb, rid, hello []byte) []byte {
	return join([]byte("echofog/hs2/prologue"), cb, rid, []byte{byte(len(hello))}, hello)
}

// Inputs fixes every random choice of one handshake run.
type Inputs struct {
	Name       string
	Hello      []byte
	Prologue   []byte
	RespS      []byte // responder hs secret
	InitS      []byte // initiator hs secret
	InitE      []byte
	RespE      []byte
	KemSeed    []byte // d ‖ z
	EncapsM    []byte
	P1         func(h1 []byte) []byte // h1 = h before EncryptAndHash(P1)
	CheckP1    func(h1, p1 []byte) error
	P2, P3     []byte
	CheckP2    func(p2, rs []byte) error // initiator, before message 3
	CheckP3    func(p3, rs []byte) error // responder, after message 3
	RespPrevS  []byte                    // responder's previous hs secret during a rotation
	RespSuites []uint16                  // responder's supported suites, most preferred first
	InitRS     []byte                    // the hs key in the initiator's copy of the card (XK)
	Responder  string                    // description only
	Initiator  string
}

type Step struct {
	Msg    int    `json:"msg"`
	Token  string `json:"token"`
	H      string `json:"h"`
	CK     string `json:"ck"`
	K      string `json:"k,omitempty"`
	N      uint64 `json:"n"`
	Detail string `json:"detail,omitempty"`
}

type Result struct {
	Steps              []Step
	Msgs               [][]byte // Noise messages 1..3 as sent or received
	H                  []byte
	CK                 []byte
	KI2R               []byte
	KR2I               []byte
	SessionSecret, SID []byte
	FailedPasses       [][]Step // responder passes over message 1 that did not decrypt P1
	Extra              map[string][]byte
	P1, P2, P3         []byte
	CardOK             map[string]bool
}

// finish is §9: Split, session_secret and sid from the final ck and h, and only then ck is
// erased. res.CK keeps a copy solely because the transcript publishes the final ck.
func finish(res *Result, h, ck []byte) {
	res.H = h
	res.KI2R, res.KR2I = noiseHKDF2(ck, nil)
	res.SessionSecret = sessionSecret(ck)
	res.SID = sessionID(res.SessionSecret, h)
	res.CK = append([]byte(nil), ck...)
	for i := range ck {
		ck[i] = 0
	}
}

type logger struct {
	r *Result
}

func (l logger) log(msg int, token string, h, ck, k []byte, n uint64, detail string) {
	s := Step{Msg: msg, Token: token, H: hexs(h), CK: hexs(ck), N: n, Detail: detail}
	if k != nil {
		s.K = hexs(k)
	}
	l.r.Steps = append(l.r.Steps, s)
}

type pipe struct {
	toResp chan []byte
	toInit chan []byte
}

func newPipe() pipe { return pipe{make(chan []byte, 1), make(chan []byte, 1)} }

func recvMsg(c chan []byte) ([]byte, error) {
	m, ok := <-c
	if !ok {
		return nil, errors.New("peer closed")
	}
	if len(m) > maxNoise {
		return nil, errors.New("noise message too long")
	}
	return m, nil
}

// =============================================================================================
// XKhfs initiator: <- s ... -> e, e1, es   <- e, ee, ekem1   -> s, se

func xkInitiator(in Inputs, rsPub []byte, p pipe) (res Result, err error) {
	defer close(p.toResp)
	res.Extra = map[string][]byte{}
	res.CardOK = map[string]bool{}
	L := logger{&res}

	h := initialH(in.Name)
	ck := append([]byte(nil), h...)
	var k []byte
	var n uint64
	L.log(0, "InitializeSymmetric", h, ck, k, n, "")
	h = mixHash(h, in.Prologue)
	L.log(0, "MixHash(prologue)", h, ck, k, n, "")
	h = mixHash(h, rsPub)
	L.log(0, "MixHash(rs) pre-message", h, ck, k, n, "")

	// ---- message 1
	var m1 []byte
	ePub := x25519Public(in.InitE)
	m1 = append(m1, ePub...)
	h = mixHash(h, ePub)
	L.log(1, "e", h, ck, k, n, "")

	dk, ek := kemGenerate(in.KemSeed)
	res.Extra["e1_ek_sha256"] = sha(ek)
	var out []byte
	out, h, n = encHash(h, k, n, ek)
	m1 = append(m1, out...)
	L.log(1, "e1", h, ck, k, n, "")

	z, err := dh(in.InitE, rsPub)
	if err != nil {
		return res, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(1, "es", h, ck, k, n, "")

	h1 := append([]byte(nil), h...)
	res.Extra["h1"] = h1
	res.P1 = in.P1(h1)
	out, h, n = encHash(h, k, n, res.P1)
	m1 = append(m1, out...)
	L.log(1, "payload", h, ck, k, n, "")
	if len(m1) > maxNoise {
		return res, errors.New("message 1 too long")
	}
	res.Msgs = append(res.Msgs, m1)
	p.toResp <- m1

	// ---- message 2
	m2, err := recvMsg(p.toInit)
	if err != nil {
		return res, err
	}
	res.Msgs = append(res.Msgs, m2)
	if len(m2) < lenPub+lenCT+tagLen+tagLen {
		return res, errors.New("message 2 too short")
	}
	re := m2[:lenPub]
	h = mixHash(h, re)
	L.log(2, "e", h, ck, k, n, "")

	z, err = dh(in.InitE, re)
	if err != nil {
		return res, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(2, "ee", h, ck, k, n, "")

	var ct []byte
	ct, h, n, err = decHash(h, k, n, m2[lenPub:lenPub+lenCT+tagLen])
	if err != nil {
		return res, fmt.Errorf("ekem1: %w", err)
	}
	ss, err := dk.Decapsulate(ct)
	if err != nil {
		return res, err
	}
	res.Extra["ekem1_ct_sha256"] = sha(ct)
	res.Extra["ekem1_ss"] = ss
	ck, k = mixKey(ck, ss)
	n = 0
	L.log(2, "ekem1", h, ck, k, n, "")

	res.P2, h, n, err = decHash(h, k, n, m2[lenPub+lenCT+tagLen:])
	if err != nil {
		return res, fmt.Errorf("P2: %w", err)
	}
	L.log(2, "payload", h, ck, k, n, "")
	if in.CheckP2 != nil {
		if err := in.CheckP2(res.P2, rsPub); err != nil {
			return res, err
		}
	}

	// ---- message 3
	var m3 []byte
	sPub := x25519Public(in.InitS)
	out, h, n = encHash(h, k, n, sPub)
	m3 = append(m3, out...)
	L.log(3, "s", h, ck, k, n, "")

	z, err = dh(in.InitS, re)
	if err != nil {
		return res, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(3, "se", h, ck, k, n, "")

	res.P3 = in.P3
	out, h, n = encHash(h, k, n, in.P3)
	m3 = append(m3, out...)
	L.log(3, "payload", h, ck, k, n, "")
	res.Msgs = append(res.Msgs, m3)
	p.toResp <- m3

	finish(&res, h, ck)
	return res, nil
}

// xkMessage1Pass is the responder's reading of XK message 1 with one static key: initialisation,
// pre-message and the tokens of message 1, ending with the P1 decryption. It is a whole pass,
// because the pre-message MixHash(rs) changes h from the start (§4.1 rotation).
type pass1 struct {
	h, ck, k  []byte
	n         uint64
	re, re1   []byte
	h1, p1    []byte
	steps     []Step
	p1Failure bool
}

func xkMessage1Pass(in Inputs, sk, m1 []byte) (ps pass1, err error) {
	var r Result
	L := logger{&r}
	defer func() { ps.steps = r.Steps }()

	h := initialH(in.Name)
	ck := append([]byte(nil), h...)
	var k []byte
	var n uint64
	L.log(0, "InitializeSymmetric", h, ck, k, n, "")
	h = mixHash(h, in.Prologue)
	L.log(0, "MixHash(prologue)", h, ck, k, n, "")
	h = mixHash(h, x25519Public(sk)) // its own hs key
	L.log(0, "MixHash(rs) pre-message", h, ck, k, n, "")

	re := m1[:lenPub]
	h = mixHash(h, re)
	L.log(1, "e", h, ck, k, n, "")

	var re1 []byte
	re1, h, n, err = decHash(h, k, n, m1[lenPub:lenPub+lenEK]) // no key yet: plaintext
	if err != nil {
		return ps, err
	}
	L.log(1, "e1", h, ck, k, n, "")

	z, err := dh(sk, re)
	if err != nil {
		return ps, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(1, "es", h, ck, k, n, "")

	h1 := append([]byte(nil), h...)
	p1, h, n, err := decHash(h, k, n, m1[lenPub+lenEK:])
	if err != nil {
		ps.p1Failure = true
		return ps, fmt.Errorf("P1: %w", err)
	}
	L.log(1, "payload", h, ck, k, n, "")
	return pass1{h: h, ck: ck, k: k, n: n, re: re, re1: re1, h1: h1, p1: p1}, nil
}

// XKhfs responder.
func xkResponder(in Inputs, p pipe) (res Result, err error) {
	defer close(p.toInit)
	res.Extra = map[string][]byte{}
	res.CardOK = map[string]bool{}
	L := logger{&res}

	if err := checkHello(in.Hello, in.RespSuites); err != nil {
		return res, err
	}

	// ---- message 1
	m1, err := recvMsg(p.toResp)
	if err != nil {
		return res, err
	}
	res.Msgs = append(res.Msgs, m1)
	if len(m1) < lenPub+lenEK+tagLen {
		return res, errors.New("message 1 too short")
	}
	keys := [][]byte{in.RespS}
	if in.RespPrevS != nil {
		keys = append(keys, in.RespPrevS) // current key first, then the previous one
	}
	var ps pass1
	for i, sk := range keys {
		ps, err = xkMessage1Pass(in, sk, m1)
		if err == nil {
			break
		}
		if !ps.p1Failure || i == len(keys)-1 {
			return res, err // one failure of §5 step 3
		}
		res.FailedPasses = append(res.FailedPasses, ps.steps)
	}
	res.Steps = ps.steps
	h, ck, k, n, re, re1 := ps.h, ps.ck, ps.k, ps.n, ps.re, ps.re1
	h1 := ps.h1
	res.Extra["h1"] = h1
	res.P1 = ps.p1
	// §5 step 4 comes only after both passes.
	if in.CheckP1 != nil {
		if err := in.CheckP1(h1, res.P1); err != nil {
			return res, err
		}
	}

	// ---- message 2
	var m2, out []byte
	ePub := x25519Public(in.RespE)
	m2 = append(m2, ePub...)
	h = mixHash(h, ePub)
	L.log(2, "e", h, ck, k, n, "")

	z, err := dh(in.RespE, re)
	if err != nil {
		return res, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(2, "ee", h, ck, k, n, "")

	ct, ss, err := kemEncap(re1, in.EncapsM) // includes the FIPS 203 ek check
	if err != nil {
		return res, err
	}
	res.Extra["ekem1_ct_sha256"] = sha(ct)
	res.Extra["ekem1_ss"] = ss
	out, h, n = encHash(h, k, n, ct)
	m2 = append(m2, out...)
	ck, k = mixKey(ck, ss)
	n = 0
	L.log(2, "ekem1", h, ck, k, n, "")

	res.P2 = in.P2
	out, h, n = encHash(h, k, n, in.P2)
	m2 = append(m2, out...)
	L.log(2, "payload", h, ck, k, n, "")
	if len(m2) > maxNoise {
		return res, errors.New("message 2 too long")
	}
	res.Msgs = append(res.Msgs, m2)
	p.toInit <- m2

	// ---- message 3
	m3, err := recvMsg(p.toResp)
	if err != nil {
		return res, err
	}
	res.Msgs = append(res.Msgs, m3)
	if len(m3) < lenPub+tagLen+tagLen {
		return res, errors.New("message 3 too short")
	}
	var rs []byte
	rs, h, n, err = decHash(h, k, n, m3[:lenPub+tagLen])
	if err != nil {
		return res, fmt.Errorf("s: %w", err)
	}
	L.log(3, "s", h, ck, k, n, "")

	z, err = dh(in.RespE, rs)
	if err != nil {
		return res, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(3, "se", h, ck, k, n, "")

	res.P3, h, n, err = decHash(h, k, n, m3[lenPub+tagLen:])
	if err != nil {
		return res, fmt.Errorf("P3: %w", err)
	}
	L.log(3, "payload", h, ck, k, n, "")
	if in.CheckP3 != nil {
		res.CardOK["p3_card_hs_key_matches_rs"] = in.CheckP3(res.P3, rs) == nil
	}

	finish(&res, h, ck)
	return res, nil
}

// =============================================================================================
// XXhfs initiator: -> e, e1   <- e, ee, ekem1, s, es   -> s, se

func xxInitiator(in Inputs, p pipe) (res Result, err error) {
	defer close(p.toResp)
	res.Extra = map[string][]byte{}
	res.CardOK = map[string]bool{}
	L := logger{&res}

	h := initialH(in.Name)
	ck := append([]byte(nil), h...)
	var k []byte
	var n uint64
	L.log(0, "InitializeSymmetric", h, ck, k, n, "")
	h = mixHash(h, in.Prologue)
	L.log(0, "MixHash(prologue)", h, ck, k, n, "")

	// ---- message 1
	var m1, out []byte
	ePub := x25519Public(in.InitE)
	m1 = append(m1, ePub...)
	h = mixHash(h, ePub)
	L.log(1, "e", h, ck, k, n, "")

	dk, ek := kemGenerate(in.KemSeed)
	res.Extra["e1_ek_sha256"] = sha(ek)
	out, h, n = encHash(h, k, n, ek)
	m1 = append(m1, out...)
	L.log(1, "e1", h, ck, k, n, "")

	h1 := append([]byte(nil), h...)
	res.Extra["h1"] = h1
	res.P1 = in.P1(h1)
	out, h, n = encHash(h, k, n, res.P1) // no key: P1 in clear, hashed
	m1 = append(m1, out...)
	L.log(1, "payload", h, ck, k, n, "")
	res.Msgs = append(res.Msgs, m1)
	p.toResp <- m1

	// ---- message 2
	m2, err := recvMsg(p.toInit)
	if err != nil {
		return res, err
	}
	res.Msgs = append(res.Msgs, m2)
	if len(m2) < lenPub+lenCT+tagLen+lenPub+tagLen+tagLen {
		return res, errors.New("message 2 too short")
	}
	off := 0
	re := m2[off : off+lenPub]
	off += lenPub
	h = mixHash(h, re)
	L.log(2, "e", h, ck, k, n, "")

	z, err := dh(in.InitE, re)
	if err != nil {
		return res, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(2, "ee", h, ck, k, n, "")

	var ct []byte
	ct, h, n, err = decHash(h, k, n, m2[off:off+lenCT+tagLen])
	off += lenCT + tagLen
	if err != nil {
		return res, fmt.Errorf("ekem1: %w", err)
	}
	ss, err := dk.Decapsulate(ct)
	if err != nil {
		return res, err
	}
	res.Extra["ekem1_ct_sha256"] = sha(ct)
	res.Extra["ekem1_ss"] = ss
	ck, k = mixKey(ck, ss)
	n = 0
	L.log(2, "ekem1", h, ck, k, n, "")

	var rs []byte
	rs, h, n, err = decHash(h, k, n, m2[off:off+lenPub+tagLen])
	off += lenPub + tagLen
	if err != nil {
		return res, fmt.Errorf("s: %w", err)
	}
	L.log(2, "s", h, ck, k, n, "")

	z, err = dh(in.InitE, rs)
	if err != nil {
		return res, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(2, "es", h, ck, k, n, "")

	res.P2, h, n, err = decHash(h, k, n, m2[off:])
	if err != nil {
		return res, fmt.Errorf("P2: %w", err)
	}
	L.log(2, "payload", h, ck, k, n, "")
	if in.CheckP2 != nil {
		if err := in.CheckP2(res.P2, rs); err != nil {
			return res, err
		}
		res.CardOK["p2_card_hs_key_matches_rs"] = true
	}

	// ---- message 3 (as XK message 3)
	var m3 []byte
	sPub := x25519Public(in.InitS)
	out, h, n = encHash(h, k, n, sPub)
	m3 = append(m3, out...)
	L.log(3, "s", h, ck, k, n, "")

	z, err = dh(in.InitS, re)
	if err != nil {
		return res, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(3, "se", h, ck, k, n, "")

	res.P3 = in.P3
	out, h, n = encHash(h, k, n, in.P3)
	m3 = append(m3, out...)
	L.log(3, "payload", h, ck, k, n, "")
	res.Msgs = append(res.Msgs, m3)
	p.toResp <- m3

	finish(&res, h, ck)
	return res, nil
}

// XXhfs responder.
func xxResponder(in Inputs, p pipe) (res Result, err error) {
	defer close(p.toInit)
	res.Extra = map[string][]byte{}
	res.CardOK = map[string]bool{}
	L := logger{&res}
	if err := checkHello(in.Hello, in.RespSuites); err != nil {
		return res, err
	}

	h := initialH(in.Name)
	ck := append([]byte(nil), h...)
	var k []byte
	var n uint64
	L.log(0, "InitializeSymmetric", h, ck, k, n, "")
	h = mixHash(h, in.Prologue)
	L.log(0, "MixHash(prologue)", h, ck, k, n, "")

	// ---- message 1
	m1, err := recvMsg(p.toResp)
	if err != nil {
		return res, err
	}
	res.Msgs = append(res.Msgs, m1)
	if len(m1) < lenPub+lenEK {
		return res, errors.New("message 1 too short")
	}
	re := m1[:lenPub]
	h = mixHash(h, re)
	L.log(1, "e", h, ck, k, n, "")

	var re1 []byte
	re1, h, n, err = decHash(h, k, n, m1[lenPub:lenPub+lenEK])
	if err != nil {
		return res, err
	}
	L.log(1, "e1", h, ck, k, n, "")

	h1 := append([]byte(nil), h...)
	res.Extra["h1"] = h1
	res.P1, h, n, err = decHash(h, k, n, m1[lenPub+lenEK:])
	if err != nil {
		return res, err
	}
	L.log(1, "payload", h, ck, k, n, "")
	if in.CheckP1 != nil {
		if err := in.CheckP1(h1, res.P1); err != nil {
			return res, err
		}
	}

	// ---- message 2
	var m2, out []byte
	ePub := x25519Public(in.RespE)
	m2 = append(m2, ePub...)
	h = mixHash(h, ePub)
	L.log(2, "e", h, ck, k, n, "")

	z, err := dh(in.RespE, re)
	if err != nil {
		return res, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(2, "ee", h, ck, k, n, "")

	ct, ss, err := kemEncap(re1, in.EncapsM)
	if err != nil {
		return res, err
	}
	res.Extra["ekem1_ct_sha256"] = sha(ct)
	res.Extra["ekem1_ss"] = ss
	out, h, n = encHash(h, k, n, ct)
	m2 = append(m2, out...)
	ck, k = mixKey(ck, ss)
	n = 0
	L.log(2, "ekem1", h, ck, k, n, "")

	sPub := x25519Public(in.RespS)
	out, h, n = encHash(h, k, n, sPub)
	m2 = append(m2, out...)
	L.log(2, "s", h, ck, k, n, "")

	z, err = dh(in.RespS, re)
	if err != nil {
		return res, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(2, "es", h, ck, k, n, "")

	res.P2 = in.P2
	out, h, n = encHash(h, k, n, in.P2)
	m2 = append(m2, out...)
	L.log(2, "payload", h, ck, k, n, "")
	if len(m2) > maxNoise {
		return res, errors.New("message 2 too long")
	}
	res.Msgs = append(res.Msgs, m2)
	p.toInit <- m2

	// ---- message 3
	m3, err := recvMsg(p.toResp)
	if err != nil {
		return res, err
	}
	res.Msgs = append(res.Msgs, m3)
	if len(m3) < lenPub+tagLen+tagLen {
		return res, errors.New("message 3 too short")
	}
	var rs []byte
	rs, h, n, err = decHash(h, k, n, m3[:lenPub+tagLen])
	if err != nil {
		return res, err
	}
	L.log(3, "s", h, ck, k, n, "")

	z, err = dh(in.RespE, rs)
	if err != nil {
		return res, err
	}
	ck, k = mixKey(ck, z)
	n = 0
	L.log(3, "se", h, ck, k, n, "")

	res.P3, h, n, err = decHash(h, k, n, m3[lenPub+tagLen:])
	if err != nil {
		return res, err
	}
	L.log(3, "payload", h, ck, k, n, "")
	if in.CheckP3 != nil {
		res.CardOK["p3_card_hs_key_matches_rs"] = in.CheckP3(res.P3, rs) == nil
	}

	finish(&res, h, ck)
	return res, nil
}

// =============================================================================================

// run executes both roles concurrently and checks that they agree on everything.
func run(xx bool, in Inputs) (ini, resp Result, err error) {
	p := newPipe()
	type out struct {
		r   Result
		err error
	}
	rc := make(chan out, 1)
	go func() {
		var r Result
		var e error
		if xx {
			r, e = xxResponder(in, p)
		} else {
			r, e = xkResponder(in, p)
		}
		rc <- out{r, e}
	}()
	var ierr error
	if xx {
		ini, ierr = xxInitiator(in, p)
	} else {
		rs := in.InitRS
		if rs == nil {
			rs = x25519Public(in.RespS)
		}
		ini, ierr = xkInitiator(in, rs, p)
	}
	ro := <-rc
	resp = ro.r
	if ierr != nil || ro.err != nil {
		return ini, resp, fmt.Errorf("initiator: %v; responder: %v", ierr, ro.err)
	}
	if len(ini.Steps) != len(resp.Steps) {
		return ini, resp, errors.New("step count differs")
	}
	for i := range ini.Steps {
		if ini.Steps[i] != resp.Steps[i] {
			return ini, resp, fmt.Errorf("step %d differs: %+v vs %+v", i, ini.Steps[i], resp.Steps[i])
		}
	}
	for i := range ini.Msgs {
		if !bytes.Equal(ini.Msgs[i], resp.Msgs[i]) {
			return ini, resp, fmt.Errorf("message %d differs", i+1)
		}
	}
	if !bytes.Equal(ini.KI2R, resp.KI2R) || !bytes.Equal(ini.KR2I, resp.KR2I) || !bytes.Equal(ini.H, resp.H) {
		return ini, resp, errors.New("final keys differ")
	}
	if !bytes.Equal(ini.P1, resp.P1) || !bytes.Equal(ini.P2, resp.P2) || !bytes.Equal(ini.P3, resp.P3) {
		return ini, resp, errors.New("payloads differ")
	}
	return ini, resp, nil
}
