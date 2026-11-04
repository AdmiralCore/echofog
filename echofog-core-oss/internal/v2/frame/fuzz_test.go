package frame

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func FuzzVarint(f *testing.F) {
	for _, s := range []string{"00", "3f", "4040", "7fff", "80004000", "c000000040000000", "4025", "80000025"} {
		f.Add(unhex(f, s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		v, n, err := ParseVarint(b)
		if err != nil {
			return
		}
		// Whatever decodes re-encodes to exactly the bytes consumed (minimality).
		if enc := AppendVarint(nil, v); !bytes.Equal(enc, b[:n]) {
			t.Fatalf("%x -> %d -> %x", b[:n], v, enc)
		}
		r, err := ReadVarint(bytes.NewReader(b))
		if err != nil || r != v {
			t.Fatalf("ReadVarint disagrees: %d %v", r, err)
		}
	})
}

func fuzzLimiter(typ uint64) (int, bool) {
	switch typ {
	case 0x40:
		return 64, true
	case 0x41:
		return 65536, true
	}
	return 0, false
}

func FuzzParseFrame(f *testing.F) {
	var v frameVectors
	loadJSON(f, "frames.json", &v)
	for _, c := range v.Frames {
		f.Add(unhex(f, c.Encoded))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fr, n, err := ParseFrame(b, fuzzLimiter)
		if err != nil && !IsUnknown(err) {
			return
		}
		if n > len(b) || len(fr.Payload) > MaxPayload {
			t.Fatalf("bad consume %d of %d", n, len(b))
		}
		// The reader agrees with the parser on known frames.
		if err == nil && Known(fr, fuzzLimiter) {
			rf, rerr := NewReader(bytes.NewReader(b[:n]), fuzzLimiter).ReadFrame()
			if rerr != nil || rf.Type != fr.Type {
				t.Fatalf("reader %v %+v vs %+v", rerr, rf, fr)
			}
			if fr.Type != TypePadding && !bytes.Equal(rf.Payload, fr.Payload) {
				t.Fatal("payload mismatch")
			}
		}
	})
}

func FuzzReader(f *testing.F) {
	var v frameVectors
	loadJSON(f, "frames.json", &v)
	var all []byte
	for _, c := range v.Frames {
		all = append(all, unhex(f, c.Encoded)...)
	}
	f.Add(all)
	f.Fuzz(func(t *testing.T, b []byte) {
		rd := NewReader(bytes.NewReader(b), fuzzLimiter)
		for i := 0; i < 1000; i++ {
			fr, err := rd.ReadFrame()
			if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) || IsTooLarge(err) || err == ErrNonMinimal {
				return
			}
			if err != nil && !IsUnknown(err) {
				t.Fatalf("unexpected error %v", err)
			}
			if err == nil {
				max, known := classify(fr.Type, fr.Flags, fuzzLimiter)
				if !known || len(fr.Payload) > max {
					t.Fatalf("returned %+v beyond max %d known=%v", fr, max, known)
				}
			}
		}
	})
}

func FuzzPayloads(f *testing.F) {
	for _, s := range []string{"030200", "090000", "04736572766963652d756e737570706f72746564", "04c0000221c822",
		"0620010db800000000000000000000000101bb", "0209", "04016869", "0405", "0480020000", "80200000",
		"0808120772657374617274181e", "080110c80118c801"} {
		f.Add(uint8(0), unhex(f, s))
	}
	f.Fuzz(func(t *testing.T, which uint8, p []byte) {
		// Each decoder either fails with ErrMalformed or round-trips canonically.
		check := func(err error, enc func() []byte) {
			if err != nil {
				if !IsMalformed(err) {
					t.Fatalf("decoder %d: non-malformed error %v", which%12, err)
				}
				return
			}
			if enc != nil {
				if e := enc(); !bytes.Equal(e, p) {
					// Only protobuf messages with unknown fields or non-canonical order may differ.
					if which%12 < 10 {
						t.Fatalf("decoder %d: %x re-encodes to %x", which%12, p, e)
					}
				}
			}
		}
		switch which % 12 {
		case 0:
			m, err := ParseOpen(p)
			check(err, func() []byte { return m.Append(nil) })
		case 1:
			m, err := ParseUnsupported(p)
			check(err, func() []byte { return m.Append(nil) })
		case 2:
			m, err := ParseClose(p)
			check(err, func() []byte { return m.Append(nil) })
		case 3:
			m, err := ParseObservedAddr(p)
			check(err, func() []byte { return AppendObservedAddr(nil, m) })
		case 4:
			m, err := ParsePathBind(p)
			check(err, func() []byte { return m.Append(nil) })
		case 5:
			m, err := ParsePathAbandon(p)
			check(err, func() []byte { return m.Append(nil) })
		case 6:
			m, err := ParseMuxStream(p)
			check(err, func() []byte { return m.Append(nil) })
		case 7:
			m, err := ParseMuxReset(p)
			check(err, func() []byte { return m.Append(nil) })
		case 8:
			m, err := ParseMuxWindow(p)
			check(err, func() []byte { return m.Append(nil) })
		case 9:
			m, err := ParsePathBindOK(p)
			check(err, func() []byte { return AppendVarint(nil, m) })
		case 10:
			_, err := ParseGoAway(p)
			check(err, nil)
		case 11:
			_, err := ParseReject(p)
			check(err, nil)
		}
	})
}

func FuzzDatagram(f *testing.F) {
	f.Add(unhex(f, "010000000000000000"))
	f.Add(unhex(f, "30404d7365616c6564e280a6"))
	f.Fuzz(func(t *testing.T, d []byte) {
		typ, p, err := ParseDatagram(d)
		if err != nil {
			return
		}
		if !bytes.Equal(AppendDatagram(nil, typ, p), d) {
			t.Fatal("datagram does not round-trip")
		}
	})
}
