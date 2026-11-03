package handshake

import (
	"bytes"
	"crypto/hmac"
	"crypto/mlkem"
	"crypto/mlkem/mlkemtest"
	"encoding/binary"
	"encoding/json"
	"hash"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
)

// TestTranscriptByHand recomputes the XKhfs and XXhfs transcripts from handshake.md §2 and §4
// with raw primitives (no internal/v2/noise), straight-line, and compares them with the file.
func TestTranscriptByHand(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(vectorDir, transcriptFile))
	if err != nil {
		t.Skip("no transcript file yet")
	}
	var f tFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, tr := range f.Transcripts {
		t.Run(tr.Name, func(t *testing.T) {
			msgs, final := byHand(t, tr)
			for i := range msgs {
				if hx(msgs[i]) != tr.Messages[i].Bytes {
					t.Fatalf("message %d differs", i+1)
				}
			}
			if final[0] != tr.Final.KI2R || final[1] != tr.Final.KR2I || final[2] != tr.Final.H {
				t.Fatal("final keys differ")
			}
		})
	}
}

type hand struct {
	t      testing.TB
	h, ck  []byte
	k      []byte
	n      uint64
	hasKey bool
}

func b2sNew() hash.Hash { h, _ := blake2s.New256(nil); return h }

func (s *hand) hash(parts ...[]byte) []byte {
	h := b2sNew()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func (s *hand) hm(key []byte, parts ...[]byte) []byte {
	m := hmac.New(b2sNew, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

func (s *hand) mixHash(d []byte) { s.h = s.hash(s.h, d) }

func (s *hand) mixKey(ikm []byte) {
	tk := s.hm(s.ck, ikm)
	s.ck = s.hm(tk, []byte{1})
	s.k = s.hm(tk, s.ck, []byte{2})
	s.n, s.hasKey = 0, true
}

func (s *hand) nonce() []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint64(b[4:], s.n)
	return b
}

func (s *hand) enc(pt []byte) []byte {
	out := pt
	if s.hasKey {
		a, _ := chacha20poly1305.New(s.k)
		out = a.Seal(nil, s.nonce(), pt, s.h)
		s.n++
	}
	s.mixHash(out)
	return out
}

func (s *hand) dh(sk, pub []byte) []byte {
	z, err := curve25519.X25519(sk, pub)
	if err != nil {
		s.t.Fatal(err)
	}
	return z
}

func byHand(t *testing.T, tr tTranscript) ([3][]byte, [3]string) {
	in := tr.Inputs
	unh := func(s string) []byte { return mustHex(t, s) }
	isk, ipub := unh(in.InitiatorStaticSK), unh(in.InitiatorStaticPub)
	rsk, rpub := unh(in.ResponderStaticSK), unh(in.ResponderStaticPub)
	ie, iePub := unh(in.InitiatorESK), unh(in.InitiatorEPub)
	re, rePub := unh(in.ResponderESK), unh(in.ResponderEPub)
	p1, p2, p3 := unh(in.P1), unh(in.P2), unh(in.P3)
	dk, err := mlkem.NewDecapsulationKey768(unh(in.E1SeedDZ))
	if err != nil {
		t.Fatal(err)
	}
	ss, ct, err := mlkemtest.Encapsulate768(dk.EncapsulationKey(), unh(in.EncapsM))
	if err != nil {
		t.Fatal(err)
	}

	s := &hand{t: t}
	s.h = s.hash([]byte(tr.ProtocolName)) // the name is longer than 32 bytes
	s.ck = s.h
	s.mixHash(unh(in.Prologue))
	xk := tr.SuiteID == SuiteXK
	if xk {
		s.mixHash(rpub)
	}
	var m [3][]byte
	// -> e, e1 [, es]
	s.mixHash(iePub)
	m[0] = append(m[0], iePub...)
	m[0] = append(m[0], s.enc(dk.EncapsulationKey().Bytes())...)
	if xk {
		s.mixKey(s.dh(ie, rpub))
		if p := s.h; hx(p) != tr.Messages[0].HBeforePayload {
			t.Fatal("h1 differs")
		}
	}
	m[0] = append(m[0], s.enc(p1)...)
	// <- e, ee, ekem1 [, s, es]
	s.mixHash(rePub)
	m[1] = append(m[1], rePub...)
	s.mixKey(s.dh(re, iePub))
	m[1] = append(m[1], s.enc(ct)...)
	s.mixKey(ss)
	if !xk {
		m[1] = append(m[1], s.enc(rpub)...)
		s.mixKey(s.dh(rsk, iePub))
	}
	m[1] = append(m[1], s.enc(p2)...)
	// -> s, se
	m[2] = append(m[2], s.enc(ipub)...)
	s.mixKey(s.dh(isk, rePub))
	m[2] = append(m[2], s.enc(p3)...)
	tk := s.hm(s.ck, nil)
	k1 := s.hm(tk, []byte{1})
	k2 := s.hm(tk, k1, []byte{2})
	if !bytes.Equal(s.dh(re, ipub), s.dh(isk, rePub)) {
		t.Fatal("se symmetric")
	}
	return m, [3]string{hx(k1), hx(k2), hx(s.h)}
}
