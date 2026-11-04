package handshake

import (
	"bytes"
	"reflect"
	"testing"
)

// Fuzz targets for every decoder of the handshake. Run one with, for example,
//
//	go test -run '^$' -fuzz FuzzParseHello ./internal/v2/handshake/

func addVectorSeeds(f *testing.F, keys ...[]string) {
	v := loadVector(f, "handshake.json")
	for _, k := range keys {
		m := obj(f, v, k[:len(k)-1]...)
		f.Add(hexField(f, m, k[len(k)-1]))
	}
}

func FuzzParseHello(f *testing.F) {
	addVectorSeeds(f, []string{"prologue_xk", "hs_hello"}, []string{"prologue_xx_bare_address", "hs_hello"},
		[]string{"puzzle", "HS_HELLO_with_solution_payload"})
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := ParseHello(b)
		if err != nil {
			return
		}
		out, err := h.Marshal()
		if err != nil || !bytes.Equal(out, b) {
			t.Fatalf("HS_HELLO has two encodings: %x -> %x (%v)", b, out, err)
		}
	})
}

func FuzzParseRetry(f *testing.F) {
	f.Add(mustHex(f, "9dc93509d0bb2bb8c9ed254591c07fab0c"))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseRetry(b)
		if err != nil {
			return
		}
		if !bytes.Equal(r.Marshal(), b) || r.Difficulty > MaxDifficulty {
			t.Fatal("HS_RETRY round trip")
		}
	})
}

// roundTrip checks that a decoded payload re-encodes to bytes that decode to the same value.
func roundTrip[T any](t *testing.T, b []byte, parse func([]byte) (*T, error), marshal func(*T) []byte) {
	v, err := parse(b)
	if err != nil {
		return
	}
	out := marshal(v)
	w, err := parse(out)
	if err != nil {
		t.Fatalf("re-encoding does not parse: %v", err)
	}
	if !reflect.DeepEqual(v, w) {
		t.Fatalf("round trip changed the value: %+v / %+v", v, w)
	}
}

func FuzzParseHsInit(f *testing.F) {
	addVectorSeeds(f, []string{"payloads", "HsInit_xk_hidden", "encoded"}, []string{"payloads", "HsInit_xx", "encoded"})
	f.Add(mustHex(f, "300108c801"))
	f.Fuzz(func(t *testing.T, b []byte) {
		roundTrip(t, b, ParseHsInit, (*HsInit).Marshal)
	})
}

func FuzzParseHsResp(f *testing.F) {
	addVectorSeeds(f, []string{"payloads", "HsResp", "encoded"}, []string{"payloads", "HsResp_xx", "encoded"})
	f.Add((&HsResp{ProtoVer: 200, Reject: &Reject{Reason: 8, Min: 1}}).Marshal())
	f.Fuzz(func(t *testing.T, b []byte) {
		roundTrip(t, b, ParseHsResp, (*HsResp).Marshal)
	})
}

func FuzzParseHsFinish(f *testing.F) {
	f.Add((&HsFinish{Card: []byte("card"), Pad: make([]byte, 7)}).Marshal())
	f.Fuzz(func(t *testing.T, b []byte) {
		roundTrip(t, b, ParseHsFinish, (*HsFinish).Marshal)
	})
}

func FuzzParseReject(f *testing.F) {
	f.Add(mustHex(f, "080110c80118c801"))
	f.Add((&Reject{Reason: ReasonSuite, Detail: "0001,0002"}).Marshal())
	f.Fuzz(func(t *testing.T, b []byte) {
		roundTrip(t, b, ParseReject, (*Reject).Marshal)
	})
}

func FuzzVerifyCard(f *testing.F) {
	c := loadVector(f, "node-card.json")
	for _, s := range []string{"public", "peer", "invite"} {
		f.Add(hexField(f, obj(f, c, s), "envelope"))
	}
	n := loadVector(f, "negative.json")
	f.Add(hexField(f, obj(f, n, "signature_s_not_canonical"), "envelope"))
	f.Add(hexField(f, obj(f, n, "small_order_signer"), "envelope"))
	f.Fuzz(func(t *testing.T, b []byte) {
		info, err := (DefaultCardVerifier{}).VerifyCard(b, fixedNow)
		if err == nil && (len(info.HsKey) != 32 || !bytes.Equal(info.Raw, b)) {
			t.Fatal("accepted card without a usable hs key")
		}
	})
}

// FuzzResponderMessage1 feeds arbitrary HS_HELLO and message 1 bytes to public and hidden
// responders. A hidden node must never answer.
func FuzzResponderMessage1(f *testing.F) {
	w := newWorld(f)
	for _, xx := range []bool{false, true} {
		ic := w.ic
		if xx {
			ic.Remote = nil
		}
		ini, _ := NewInitiator(ic, testCB)
		h, m, _ := ini.Start()
		f.Add(h, m)
	}
	f.Fuzz(func(t *testing.T, hello, m1 []byte) {
		rep := NewResponder(w.rc).HandleFirst(testCB, hello, m1, fixedNow)
		if rep.Action == ActSend && rep.FrameType == FrameMsg && len(rep.Payload) < 1152 {
			t.Fatal("short message 2")
		}
		hc := w.rc
		hc.Hidden, hc.Passes = true, passMap{}
		rep = NewResponder(hc).HandleFirst(testCB, hello, m1, fixedNow)
		if rep.Action != ActSilentClose || rep.Payload != nil {
			t.Fatalf("hidden node without passes answered: %v", rep.Action)
		}
	})
}

// FuzzInitiatorMessage2 feeds arbitrary message 2 bytes to an initiator after a real message 1.
func FuzzInitiatorMessage2(f *testing.F) {
	w := newWorld(f)
	ini, _ := NewInitiator(w.ic, testCB)
	h, m1, _ := ini.Start()
	rep := NewResponder(w.rc).HandleFirst(testCB, h, m1, fixedNow)
	f.Add(rep.Payload)
	f.Fuzz(func(t *testing.T, m2 []byte) {
		ini, _ := NewInitiator(w.ic, testCB)
		ini.Start()
		if m3, _, err := ini.HandleMsg2(m2); err == nil && len(m3) < 64 {
			t.Fatal("short message 3")
		}
	})
}

// FuzzResponderMessage3 feeds arbitrary message 3 bytes after a real exchange.
func FuzzResponderMessage3(f *testing.F) {
	w := newWorld(f)
	ini, _ := NewInitiator(w.ic, testCB)
	h, m1, _ := ini.Start()
	res := NewResponder(w.rc)
	rep := res.HandleFirst(testCB, h, m1, fixedNow)
	m3, _, _ := ini.HandleMsg2(rep.Payload)
	f.Add(m3)
	f.Fuzz(func(t *testing.T, b []byte) {
		ini, _ := NewInitiator(w.ic, testCB)
		h, m1, _ := ini.Start()
		res := NewResponder(w.rc)
		rep := res.HandleFirst(testCB, h, m1, fixedNow)
		if rep.Action != ActSend {
			t.Fatal(rep.Err)
		}
		res.HandleFinish(b)
	})
}
