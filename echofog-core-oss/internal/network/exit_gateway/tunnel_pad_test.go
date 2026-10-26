package exit_gateway

import (
	"bytes"
	crand "crypto/rand"
	"io"
	"net"
	"testing"

	"github.com/echofog/echofog-core/internal/network/net_types"
)

// capStream records every underlying Write (one per chunk in the padded phase) and serves a fixed
// reader. It satisfies FramedStream; the frame methods are unused in the data phase.
type capStream struct {
	w      bytes.Buffer
	writes []int
	r      io.Reader
}

func (s *capStream) Write(p []byte) (int, error) {
	s.writes = append(s.writes, len(p))
	return s.w.Write(p)
}
func (s *capStream) Read(p []byte) (int, error) {
	if s.r == nil {
		return 0, io.EOF
	}
	return s.r.Read(p)
}
func (s *capStream) Close() error                                      { return nil }
func (s *capStream) WriteFrame(net_types.MessageType, []byte) error    { return nil }
func (s *capStream) ReadFrame() (net_types.MessageType, []byte, error) { return 0, nil, io.EOF }

// padWrite writes every chunk through a fresh padding writer and returns the wire bytes plus the
// per-chunk write sizes.
func padWrite(t *testing.T, writes [][]byte) ([]byte, []int) {
	t.Helper()
	cs := &capStream{}
	w := newTunnelPad(cs)
	for i, p := range writes {
		n, err := w.Write(p)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if n != len(p) {
			t.Fatalf("write %d: wrote %d of %d", i, n, len(p))
		}
	}
	return cs.w.Bytes(), cs.writes
}

// padRead reads the whole wire back through a padding reader, readBuf bytes at a time.
func padRead(t *testing.T, wire []byte, readBuf int) ([]byte, error) {
	t.Helper()
	r := newTunnelPad(&capStream{r: bytes.NewReader(wire)})
	var out []byte
	buf := make([]byte, readBuf)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

func TestTunnelPad_RoundTrip(t *testing.T) {
	mkrand := func(n int) []byte { b := make([]byte, n); _, _ = crand.Read(b); return b }
	cases := map[string][][]byte{
		"one small":       {mkrand(517)},
		"handshake-ish":   {mkrand(517), mkrand(60), mkrand(300)},
		"crosses budget":  {mkrand(3000), mkrand(3000)},
		"one big":         {mkrand(5000)},
		"big then small":  {mkrand(9000), mkrand(40), mkrand(1500)},
		"many tiny":       {mkrand(1), mkrand(1), mkrand(1), mkrand(2), mkrand(900)},
		"empty then data": {{}, mkrand(517)},
	}
	for name, writes := range cases {
		for _, rb := range []int{1, 7, 64, 4096, 1 << 20} {
			wire, _ := padWrite(t, writes)
			got, err := padRead(t, wire, rb)
			if err != nil {
				t.Fatalf("%s rb=%d: read: %v", name, rb, err)
			}
			var want []byte
			for _, p := range writes {
				want = append(want, p...)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s rb=%d: round trip mismatch: got %d bytes, want %d", name, rb, len(got), len(want))
			}
		}
	}
}

// the detector window on the wire; a framed chunk (before QUIC's own ~40 B) must stay clear of it.
const (
	detectLo = 533
	detectHi = 837
)

func TestTunnelPad_InflatesOutOfWindow(t *testing.T) {
	// every chunk that carries a window-sized inner record must be inflated above the window and
	// stay inside one datagram (<= padCeiling).
	for _, dataLen := range []int{1, 60, 490, 517, 700, 850} {
		_, sizes := padWrite(t, [][]byte{make([]byte, dataLen)})
		if len(sizes) == 0 {
			t.Fatalf("dataLen=%d: no chunk written", dataLen)
		}
		c := sizes[0]
		if c <= detectHi {
			t.Errorf("dataLen=%d: chunk %d B is inside/below the detector window (<= %d)", dataLen, c, detectHi)
		}
		if c > padCeiling {
			t.Errorf("dataLen=%d: chunk %d B exceeds the single-datagram ceiling %d", dataLen, c, padCeiling)
		}
	}
}

func TestTunnelPad_RawAfterBudget(t *testing.T) {
	// one write past the budget: the chunk that crosses it is final, the remainder is raw, and
	// later writes are raw (recorded size == data size, no header/pad).
	_, sizes := padWrite(t, [][]byte{make([]byte, padBudget+1000), make([]byte, 2000)})
	// sizes[0] = framed chunk of padMaxChunk data (+header+jitter); then raw remainders.
	if sizes[0] <= padMaxChunk { // must carry header + pad on top of 4096 data
		t.Errorf("first chunk %d B is not a framed 4K chunk", sizes[0])
	}
	foundRaw2000 := false
	for _, s := range sizes[1:] {
		if s == 2000 {
			foundRaw2000 = true
		}
		if s > padMaxChunk+padCeiling {
			continue // bulk raw write, fine
		}
	}
	if !foundRaw2000 {
		t.Errorf("the second write did not pass through raw as 2000 B: sizes=%v", sizes)
	}
}

func TestTunnelPad_CleanEOFAtBoundary(t *testing.T) {
	// all chunks stay under the budget, so none is marked final; a clean close ends the reader on
	// io.EOF at a chunk boundary, not io.ErrUnexpectedEOF.
	writes := [][]byte{make([]byte, 100), make([]byte, 200)}
	wire, _ := padWrite(t, writes)
	got, err := padRead(t, wire, 64)
	if err != nil {
		t.Fatalf("clean EOF should read without error, got %v", err)
	}
	if len(got) != 300 {
		t.Fatalf("got %d bytes, want 300", len(got))
	}
}

func TestTunnelPad_TruncatedHeader(t *testing.T) {
	r := newTunnelPad(&capStream{r: bytes.NewReader([]byte{0, 1})}) // 2 of 5 header bytes
	_, err := r.Read(make([]byte, 64))
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated header: got %v, want ErrUnexpectedEOF", err)
	}
}

func TestTunnelPad_TruncatedPad(t *testing.T) {
	// header promises 4 data + 100 pad, but the pad is cut short.
	var wire []byte
	wire = append(wire, padCmdData, 0, 4, 0, 100) // cmd, dataLen=4, padLen=100
	wire = append(wire, 1, 2, 3, 4)               // the 4 data bytes
	wire = append(wire, make([]byte, 10)...)      // only 10 of 100 pad bytes
	got, err := padRead(t, wire, 64)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated pad: got err %v, want ErrUnexpectedEOF", err)
	}
	if !bytes.Equal(got, []byte{1, 2, 3, 4}) {
		t.Fatalf("truncated pad: delivered %v, want the 4 data bytes", got)
	}
}

// TestTunnelPad_PipeBothDirections exercises the wrapper the way the splice does: two goroutines
// copying through net.Pipe-backed padded streams in both directions at once.
func TestTunnelPad_PipeBothDirections(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	pa := newTunnelPad(connFramed{a})
	pb := newTunnelPad(connFramed{b})

	payload := make([]byte, 50000)
	_, _ = crand.Read(payload)

	done := make(chan []byte, 1)
	go func() {
		got := make([]byte, 0, len(payload))
		buf := make([]byte, 4096)
		for len(got) < len(payload) {
			n, err := pb.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				break
			}
		}
		done <- got
	}()

	off := 0
	for off < len(payload) {
		end := off + 1200
		if end > len(payload) {
			end = len(payload)
		}
		if _, err := pa.Write(payload[off:end]); err != nil {
			t.Fatalf("write: %v", err)
		}
		off = end
	}
	got := <-done
	if !bytes.Equal(got, payload) {
		t.Fatalf("pipe round trip mismatch: got %d of %d bytes", len(got), len(payload))
	}
}

// connFramed adapts a net.Conn to FramedStream (the frame methods are unused in the data phase).
type connFramed struct{ net.Conn }

func (connFramed) WriteFrame(net_types.MessageType, []byte) error    { return nil }
func (connFramed) ReadFrame() (net_types.MessageType, []byte, error) { return 0, nil, io.EOF }

func FuzzTunnelPadReader(f *testing.F) {
	f.Add([]byte{padCmdData, 0, 4, 0, 2, 1, 2, 3, 4, 9, 9})
	f.Add([]byte{padCmdFinal, 0, 0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, wire []byte) {
		r := newTunnelPad(&capStream{r: bytes.NewReader(wire)})
		buf := make([]byte, 333)
		for i := 0; i < 10000; i++ {
			if _, err := r.Read(buf); err != nil {
				return // any error (incl. EOF/ErrUnexpectedEOF) is fine; it must not panic or loop forever
			}
		}
	})
}
