package frame

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"
)

func TestSessionRegistryMaxima(t *testing.T) {
	// registries.md §1, "Max payload" column.
	want := map[uint64]int{
		TypePadding: 16384, TypeHsHello: 64, TypeHsMsg: 16384, TypeHsRetry: 32, TypeReject: 512,
		TypePing: 8, TypePong: 8, TypeGoAway: 256, TypeClose: 210, TypeOpen: 24, TypeUnsupported: 24,
		TypeObservedAddr: 19, TypeCardUpdate: 4096, TypePathBind: 56, TypePathBindOK: 8,
		TypePathAbandon: 16, TypePunchOffer: 256, TypePunchSync: 8, TypeMuxStream: 16400,
		TypeMuxReset: 16, TypeMuxWindow: 16, TypeMuxMaxData: 8, TypeMuxDatagram: 1452,
	}
	for typ, max := range want {
		info, ok := SessionType(typ)
		if !ok || info.Max != max {
			t.Errorf("type %#x: %+v %v, want max %d", typ, info, ok, max)
		}
	}
	for typ := uint64(0); typ < ServiceLocalMin; typ++ {
		if _, ok := want[typ]; ok {
			continue
		}
		if _, ok := SessionType(typ); ok {
			t.Errorf("type %#x is known but not in registries.md §1", typ)
		}
	}
	if g, ok := SessionType(TypeGrease); ok {
		t.Errorf("GREASE must never be a known type on receipt: %+v", g)
	}
}

func TestLengthLimits(t *testing.T) {
	// A known type: one byte over its maximum is frame-too-large, before reading the payload.
	over := AppendFrame(nil, Frame{Type: TypePing, Payload: make([]byte, 9)})
	if _, _, err := ParseFrame(over, nil); !IsTooLarge(err) {
		t.Fatalf("PING with 9 bytes: %v", err)
	}
	ok := AppendFrame(nil, Frame{Type: TypePing, Payload: make([]byte, 8)})
	if _, _, err := ParseFrame(ok, nil); err != nil {
		t.Fatalf("PING with 8 bytes: %v", err)
	}
	// Every known type at max and max+1.
	for typ := uint64(0); typ < ServiceLocalMin; typ++ {
		info, known := SessionType(typ)
		if !known {
			continue
		}
		at := AppendFrame(nil, Frame{Type: typ, Payload: make([]byte, info.Max)})
		if _, _, err := ParseFrame(at, nil); err != nil {
			t.Errorf("%s at max: %v", info.Name, err)
		}
		hdr := AppendFrameHeader(nil, typ, 0, uint64(info.Max+1))
		if _, _, err := ParseFrame(hdr, nil); !IsTooLarge(err) {
			t.Errorf("%s over max (header only): %v", info.Name, err)
		}
	}
	// Unknown types: never more than 1 MiB.
	hdr := AppendFrameHeader(nil, 0x3d, FlagIgnorable, MaxPayload+1)
	if _, _, err := ParseFrame(hdr, nil); !IsTooLarge(err) {
		t.Fatalf("unknown over 1 MiB: %v", err)
	}
	// A service limiter that admits more than 1 MiB is still capped.
	lim := func(typ uint64) (int, bool) { return 4 << 20, true }
	hdr = AppendFrameHeader(nil, 0x40, 0, MaxPayload+1)
	if _, _, err := ParseFrame(hdr, lim); !IsTooLarge(err) {
		t.Fatalf("service type over 1 MiB: %v", err)
	}
}

// countingReader records how many bytes were pulled; the frame reader must not read past a frame.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestReaderUnknownAndPadding(t *testing.T) {
	var b []byte
	b = AppendFrame(b, Frame{Type: TypeGrease, Flags: FlagIgnorable, Payload: []byte{1, 2, 3}})
	b = AppendFrame(b, Frame{Type: 0x3d, Flags: FlagIgnorable, Payload: make([]byte, 1000)})
	// A known type with a reserved flag bit is treated as unknown; with IGNORABLE it is skipped.
	b = AppendFrame(b, Frame{Type: TypePing, Flags: FlagIgnorable | 0x80, Payload: make([]byte, 8)})
	b = AppendFrame(b, Frame{Type: TypePadding, Flags: FlagIgnorable, Payload: make([]byte, 300)})
	b = AppendFrame(b, Frame{Type: TypePing, Payload: []byte("12345678")})
	// unknown, not ignorable
	b = AppendFrame(b, Frame{Type: 0x3d, Payload: []byte("xx")})
	// a known type with a reserved flag and no IGNORABLE: unknown, not ignorable
	b = AppendFrame(b, Frame{Type: TypePing, Flags: 0x02, Payload: []byte("12345678")})
	b = AppendFrame(b, Frame{Type: TypePong, Payload: []byte("abcdefgh")})

	cr := &countingReader{r: bytes.NewReader(b)}
	rd := NewReader(cr, nil)
	f, err := rd.ReadFrame()
	if err != nil || f.Type != TypePadding || f.Payload != nil {
		t.Fatalf("want PADDING without payload, got %+v %v", f, err)
	}
	if rd.Skipped() != 3 {
		t.Fatalf("skipped %d, want 3", rd.Skipped())
	}
	f, err = rd.ReadFrame()
	if err != nil || f.Type != TypePing || string(f.Payload) != "12345678" {
		t.Fatalf("PING: %+v %v", f, err)
	}
	_, err = rd.ReadFrame()
	var ue *UnknownFrameError
	if !errors.As(err, &ue) || ue.Type != 0x3d {
		t.Fatalf("want UnknownFrameError, got %v", err)
	}
	_, err = rd.ReadFrame()
	if !errors.As(err, &ue) || ue.Type != TypePing {
		t.Fatalf("reserved flag: want UnknownFrameError, got %v", err)
	}
	// The stream is still in sync after an unknown frame (its payload was consumed).
	f, err = rd.ReadFrame()
	if err != nil || f.Type != TypePong {
		t.Fatalf("PONG: %+v %v", f, err)
	}
	if cr.n != len(b) {
		t.Fatalf("read %d of %d bytes", cr.n, len(b))
	}
	if _, err := rd.ReadFrame(); err != io.EOF {
		t.Fatalf("end: %v", err)
	}
}

func TestReaderDoesNotOverread(t *testing.T) {
	a := AppendFrame(nil, Frame{Type: TypePing, Payload: []byte("12345678")})
	b := append(append([]byte{}, a...), 0xff, 0xff, 0xff)
	cr := &countingReader{r: bytes.NewReader(b)}
	if _, err := NewReader(cr, nil).ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if cr.n != len(a) {
		t.Fatalf("reader consumed %d bytes, frame is %d", cr.n, len(a))
	}
}

func TestReaderTruncated(t *testing.T) {
	a := AppendFrame(nil, Frame{Type: TypePing, Payload: []byte("12345678")})
	for i := 1; i < len(a); i++ {
		_, err := NewReader(bytes.NewReader(a[:i]), nil).ReadFrame()
		if err != io.ErrUnexpectedEOF {
			t.Fatalf("cut at %d: %v", i, err)
		}
		if _, _, err := ParseFrame(a[:i], nil); err != ErrShort {
			t.Fatalf("parse cut at %d: %v", i, err)
		}
	}
}

// A reader never allocates the declared length up front for a large frame that never arrives.
func TestReaderBoundedAllocation(t *testing.T) {
	lim := func(typ uint64) (int, bool) { return MaxPayload, true }
	hdr := AppendFrameHeader(nil, 0x45, 0, MaxPayload)
	rd := NewReader(bytes.NewReader(append(hdr, make([]byte, 100)...)), lim)
	if _, err := rd.ReadFrame(); err != io.ErrUnexpectedEOF {
		t.Fatalf("err %v", err)
	}
	if rd.maxBuf > readChunk {
		t.Fatalf("reader grew its buffer to %d before any payload arrived", rd.maxBuf)
	}
}

func TestServiceLocalLimiter(t *testing.T) {
	lim := func(typ uint64) (int, bool) {
		if typ == 0x40 {
			return 64, true
		}
		return 0, false
	}
	ok := AppendFrame(nil, Frame{Type: 0x40, Payload: make([]byte, 64)})
	if _, _, err := ParseFrame(ok, lim); err != nil {
		t.Fatal(err)
	}
	over := AppendFrame(nil, Frame{Type: 0x40, Payload: make([]byte, 65)})
	if _, _, err := ParseFrame(over, lim); !IsTooLarge(err) {
		t.Fatalf("%v", err)
	}
	// 0x41 is not known to this limiter: unknown, skipped with IGNORABLE, error without.
	var b []byte
	b = AppendFrame(b, Frame{Type: 0x41, Flags: FlagIgnorable, Payload: make([]byte, 100)})
	b = AppendFrame(b, Frame{Type: 0x41, Payload: make([]byte, 100)})
	rd := NewReader(bytes.NewReader(b), lim)
	_, err := rd.ReadFrame()
	var ue *UnknownFrameError
	if !errors.As(err, &ue) || ue.Type != 0x41 || rd.Skipped() != 1 {
		t.Fatalf("%v skipped=%d", err, rd.Skipped())
	}
	// Without a limiter, service-local types are unknown.
	if _, _, err := ParseFrame(ok, nil); !errors.As(err, &ue) {
		t.Fatalf("nil limiter: %v", err)
	}
}

func TestPayloadCodecsRejectMalformed(t *testing.T) {
	bad := []struct {
		name string
		fn   func() error
	}{
		{"open trailing", func() error { _, err := ParseOpen([]byte{3, 2, 0, 0}); return err }},
		{"open short", func() error { _, err := ParseOpen([]byte{3, 2}); return err }},
		{"open non-minimal", func() error { _, err := ParseOpen([]byte{0x40, 3, 2, 0}); return err }},
		{"unsupported short", func() error { _, err := ParseUnsupported([]byte{9, 0}); return err }},
		{"close long reason", func() error {
			_, err := ParseClose(append([]byte{1}, strings.Repeat("x", 201)...))
			return err
		}},
		{"close bad utf8", func() error { _, err := ParseClose([]byte{1, 0xff, 0xfe}); return err }},
		{"ping 7", func() error { _, err := ParsePing(make([]byte, 7)); return err }},
		{"observed family", func() error { _, err := ParseObservedAddr([]byte{5, 1, 2, 3, 4, 0, 1}); return err }},
		{"observed v4 len", func() error { _, err := ParseObservedAddr([]byte{4, 1, 2, 3, 4, 0}); return err }},
		{"path_bind short", func() error { _, err := ParsePathBind(make([]byte, 48)); return err }},
		{"path_bind trailing", func() error { _, err := ParsePathBind(make([]byte, 50)); return err }},
		{"mux_stream data", func() error {
			_, err := ParseMuxStream(append([]byte{4, 0}, make([]byte, MaxMuxData+1)...))
			return err
		}},
		{"mux_stream no flags", func() error { _, err := ParseMuxStream([]byte{4}); return err }},
		{"mux_reset trailing", func() error { _, err := ParseMuxReset([]byte{4, 5, 6}); return err }},
		{"goaway bad wire type", func() error { _, err := ParseGoAway([]byte{0x0a, 0x01, 0x00}); return err }},
		{"goaway truncated", func() error { _, err := ParseGoAway([]byte{0x12, 0x05, 'a'}); return err }},
		{"reject detail too long", func() error {
			m := Reject{Reason: 5, Detail: strings.Repeat("d", 201)}
			_, err := ParseReject(m.Append(nil))
			return err
		}},
	}
	for _, c := range bad {
		if err := c.fn(); err == nil {
			t.Errorf("%s: accepted", c.name)
		} else if !IsMalformed(err) {
			t.Errorf("%s: error %v is not ErrMalformed", c.name, err)
		}
	}
}

// Unknown protobuf fields are skipped (versioning.md §3.5), so a v2.1 sender can add fields.
func TestProtobufUnknownFieldsSkipped(t *testing.T) {
	g := GoAway{Code: CodeGoingAway, Reason: "restart", RetryAfter: 30}
	b := g.Append(nil)
	b = append(b, 0x20, 0x07)                  // field 4 varint 7
	b = append(b, 0x2a, 0x03, 'n', 'e', 'w')   // field 5 bytes
	b = append(b, 0x35, 1, 2, 3, 4)            // field 6 fixed32
	b = append(b, 0x39, 1, 2, 3, 4, 5, 6, 7, 8) // field 7 fixed64
	got, err := ParseGoAway(b)
	if err != nil || got != g {
		t.Fatalf("%+v %v", got, err)
	}
	r := Reject{Reason: 4, RetryAfter: 9, Detail: "busy"}
	rb := append(r.Append(nil), 0x48, 0x01) // field 9
	gr, err := ParseReject(rb)
	if err != nil || gr != r {
		t.Fatalf("%+v %v", gr, err)
	}
}

func TestObservedAddrRoundTrip(t *testing.T) {
	for _, s := range []string{"192.0.2.33:51234", "[2001:db8::1]:443"} {
		ap := netip.MustParseAddrPort(s)
		p, err := ParseObservedAddr(AppendObservedAddr(nil, ap))
		if err != nil || p != ap {
			t.Fatalf("%s: %v %v", s, p, err)
		}
	}
}

func TestDatagramErrors(t *testing.T) {
	if _, _, err := ParseDatagram(nil); err == nil {
		t.Fatal("empty datagram accepted")
	}
	if _, _, err := ParseDatagram([]byte{0x40, 0x01}); err != ErrNonMinimal {
		t.Fatalf("non-minimal datagram type: %v", err)
	}
	if !DatagramKnown(DgramSealed) || DatagramKnown(0x77) {
		t.Fatal("datagram registry")
	}
}

func TestGrease(t *testing.T) {
	n := 0
	for v := 0; v <= 0xffff; v++ {
		if IsGrease16(uint16(v)) {
			n++
			if v&0x0f0f != 0x0a0a || v>>12 != (v>>4)&0xf || v>>12 > 0xe {
				t.Fatalf("%#04x is not of the form 0xXAXA", v)
			}
		}
	}
	if n != 15 {
		t.Fatalf("%d GREASE values, want 15", n)
	}
	if IsGrease16(0xfafa) {
		t.Fatal("0xfafa is experimental, not GREASE")
	}
}
