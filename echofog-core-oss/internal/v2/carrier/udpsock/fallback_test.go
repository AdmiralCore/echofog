package udpsock

import (
	"bytes"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

func readMessages(t *testing.T, c *Conn, want int) [][]byte {
	t.Helper()
	var out [][]byte
	ms := messages(want, 4096)
	for len(out) < want {
		if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatalf("deadline: %v", err)
		}
		n, err := c.ReadBatch(ms, 0)
		if err != nil {
			t.Fatalf("read after %d of %d datagrams: %v", len(out), want, err)
		}
		for i := 0; i < n; i++ {
			out = append(out, bytes.Clone(ms[i].Buffers[0][:ms[i].N]))
		}
	}
	return out
}

// TestReadBatchFailureFallsBack proves the first refused batch read is permanent for the socket
// and that the datagram already queued is delivered, not lost, by the single-call path.
func TestReadBatchFailureFallsBack(t *testing.T) {
	a := listen(t, Options{})
	b := listen(t, Options{})
	if !a.batchRead.Load() {
		t.Skip("the platform has no batch read")
	}
	refused := errors.New("recvmmsg refused")
	var calls atomic.Int32
	a.readKernelFn = func(ms []ipv4.Message, flags int) (int, error) {
		calls.Add(1)
		return 0, refused
	}

	if _, err := b.WriteTo([]byte("first"), addrOf(t, a)); err != nil {
		t.Fatalf("write: %v", err)
	}
	ms := messages(1, 64)
	n, err := a.ReadBatch(ms, 0)
	if err != nil || n != 1 {
		t.Fatalf("read after a refused batch: %d %v", n, err)
	}
	if got := string(ms[0].Buffers[0][:ms[0].N]); got != "first" {
		t.Fatalf("payload %q, want %q", got, "first")
	}
	if a.batchRead.Load() {
		t.Fatal("batch read is still on after the kernel refused it")
	}

	if _, err := b.WriteTo([]byte("second"), addrOf(t, a)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if n, err := a.ReadBatch(ms, 0); err != nil || n != 1 {
		t.Fatalf("second read: %d %v", n, err)
	}
	if got := string(ms[0].Buffers[0][:ms[0].N]); got != "second" {
		t.Fatalf("payload %q, want %q", got, "second")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("batch read tried %d times, want 1: the fallback must be permanent", got)
	}
}

// TestGSORefusedFallsBackWithoutLoss proves a refused GSO train is resent one datagram at a time
// and that GSO is never tried on that socket again.
func TestGSORefusedFallsBackWithoutLoss(t *testing.T) {
	a := listen(t, Options{})
	b := listen(t, Options{})
	if !a.gso.Load() {
		t.Skip("the platform has no GSO")
	}
	dst := addrOf(t, b)
	refused := errors.New("UDP_SEGMENT refused")
	var trains atomic.Int32
	a.writeMsgFn = func(p, oob []byte, addr *net.UDPAddr) (int, int, error) {
		if len(p) > 100 {
			trains.Add(1)
			return 0, 0, refused
		}
		return a.writeRaw(p, oob, addr)
	}
	batch := func(base byte) [][]byte {
		bufs := make([][]byte, 8)
		for i := range bufs {
			bufs[i] = bytes.Repeat([]byte{base + byte(i)}, 100)
		}
		return bufs
	}

	first := batch(1)
	n, err := a.WriteBatch(dst, first)
	if err != nil || n != len(first) {
		t.Fatalf("batch write after a refused train: %d %v", n, err)
	}
	if a.gso.Load() {
		t.Fatal("GSO is still on after the kernel refused a train")
	}
	if got := trains.Load(); got != 1 {
		t.Fatalf("GSO trains tried %d times, want 1", got)
	}
	if got := readMessages(t, b, len(first)); !equal(got, first) {
		t.Fatalf("first batch arrived as %v, want %v", got, first)
	}

	second := batch(11)
	if n, err := a.WriteBatch(dst, second); err != nil || n != len(second) {
		t.Fatalf("second batch: %d %v", n, err)
	}
	if got := trains.Load(); got != 1 {
		t.Fatalf("GSO was tried again after the fallback: %d trains", got)
	}
	if got := readMessages(t, b, len(second)); !equal(got, second) {
		t.Fatalf("second batch arrived as %v, want %v", got, second)
	}
}

// TestWriteBatchHonoursErrors checks a single write error is reported after the datagrams that
// did go out, not swallowed.
func TestWriteBatchReportsWriteError(t *testing.T) {
	a := listen(t, Options{})
	b := listen(t, Options{})
	dst := addrOf(t, b)
	boom := errors.New("write failed")
	var calls atomic.Int32
	a.writeMsgFn = func(p, oob []byte, addr *net.UDPAddr) (int, int, error) {
		if calls.Add(1) == 3 {
			return 0, 0, boom
		}
		return a.writeRaw(p, oob, addr)
	}
	bufs := [][]byte{[]byte("aaa"), []byte("bbb"), []byte("ccc")}
	n, err := a.WriteBatch(dst, bufs)
	if !errors.Is(err, boom) {
		t.Fatalf("write: %v, want %v", err, boom)
	}
	if n != 2 {
		t.Fatalf("%d datagrams reported as sent, want 2", n)
	}
	if got := readMessages(t, b, 2); len(got) != 2 || string(got[0]) != "aaa" || string(got[1]) != "bbb" {
		t.Fatalf("arrived %q", got)
	}
}

func equal(got, want [][]byte) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			return false
		}
	}
	return true
}
