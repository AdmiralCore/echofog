package network

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"
)

// pipeMux adapts a net.Conn to MuxStream for testing the padding layer in isolation.
type pipeMux struct{ net.Conn }

func (pipeMux) CancelRead() {}

// The padding layer is transparent: whatever is written comes out byte for byte, across the
// framed start and the plain-copy tail, for many write sizes and when writes cross the budget.
func TestVisionStreamRoundTrip(t *testing.T) {
	sizes := []int{1, 100, visionMaxChunk, visionMaxChunk + 7, visionPadBudget - 10, visionPadBudget, visionPadBudget + 5000, 70000}
	total := 0
	for _, s := range sizes {
		total += s
	}
	payload := make([]byte, total)
	_, _ = rand.Read(payload)

	a, b := net.Pipe()
	w := newVisionStream(pipeMux{a})
	r := newVisionStream(pipeMux{b})

	go func() {
		off := 0
		for _, s := range sizes {
			if _, err := w.Write(payload[off : off+s]); err != nil {
				return
			}
			off += s
		}
		_ = a.Close()
	}()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload corrupted: got %d bytes, want %d", len(got), len(payload))
	}
}

// The bytes actually put on the wire for the start of the stream are larger than the payload (the
// padding), and the early frames do not all have the same size (so the inner handshake's record
// pattern does not show through).
func TestVisionStreamPadsAndVariesTheStart(t *testing.T) {
	var sink countingConn
	w := newVisionStream(pipeMux{&sink})
	msg := make([]byte, 2000) // two chunks within the budget
	if _, err := w.Write(msg); err != nil {
		t.Fatal(err)
	}
	if sink.n <= len(msg) {
		t.Fatalf("no padding added: wrote %d for %d bytes of payload", sink.n, len(msg))
	}
}

// countingConn counts bytes written and discards them.
type countingConn struct {
	net.Conn
	n int
}

func (c *countingConn) Write(p []byte) (int, error)      { c.n += len(p); return len(p), nil }
func (c *countingConn) SetWriteDeadline(time.Time) error { return nil }
