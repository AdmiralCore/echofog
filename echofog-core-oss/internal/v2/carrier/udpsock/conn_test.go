package udpsock

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

// totalDatagrams is how many datagrams the batched and the single path must each carry, as the
// task requires: 10 000 datagrams of different sizes, same result on both paths.
const totalDatagrams = 10000

// recvWindow is how many datagrams may be in flight in the loopback test: more than the kernel
// receive buffer can drop, so a lost datagram is a failure and not a race with the sender.
const recvWindow = 1024

// testRecvBuffer is what the loopback pair asks of SO_RCVBUF. The kernel doubles the request and
// caps it at net.core.rmem_max, which is 208 KiB on a stock Ubuntu runner: asking 104 KiB leaves
// exactly the 208 KiB ceiling CI has, so a local run sees the runner's buffer instead of a 4 MiB
// one. The old 1024-datagram window does not fit it: 208 KiB holds 92 datagrams of 1400 bytes.
const testRecvBuffer = 104 << 10

// maxWriteBatch is the longest train the test asks for; the kernel accepts 64 segments.
const maxWriteBatch = 32

var payloadSizes = [...]int{8, 16, 48, 100, 256, 600, 1200, 1400}

// allPayloads holds the whole sequence once; both runs send exactly the same bytes.
var allPayloads = makePayloads()

func makePayloads() [][]byte {
	ps := make([][]byte, totalDatagrams)
	for i := range ps {
		size := payloadSizes[i*len(payloadSizes)/totalDatagrams]
		b := make([]byte, size)
		binary.BigEndian.PutUint64(b, uint64(i))
		x := uint64(i)*0x9e3779b97f4a7c15 + 1
		for j := 8; j < len(b); j++ {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			b[j] = byte(x)
		}
		ps[i] = b
	}
	return ps
}

// checkPayload tells which datagram b is and whether it is intact.
func checkPayload(b []byte) (int, bool) {
	if len(b) < 8 {
		return 0, false
	}
	i := int(binary.BigEndian.Uint64(b))
	if i < 0 || i >= totalDatagrams || !bytes.Equal(allPayloads[i], b) {
		return 0, false
	}
	return i, true
}

func listen(t *testing.T, opt Options) *Conn {
	t.Helper()
	c, err := Listen(context.Background(), "udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, opt)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func pair(t *testing.T, a, b Options) (*Conn, *Conn) {
	t.Helper()
	return listen(t, a), listen(t, b)
}

func addrOf(t *testing.T, c *Conn) *net.UDPAddr {
	t.Helper()
	a, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("local address %T is not a UDP address", c.LocalAddr())
	}
	return a
}

func messages(n, size int) []ipv4.Message {
	ms := make([]ipv4.Message, n)
	for i := range ms {
		ms[i].Buffers = [][]byte{make([]byte, size)}
		ms[i].OOB = make([]byte, 128)
	}
	return ms
}

func TestRoundTripBothWays(t *testing.T) {
	cases := []struct {
		name string
		opt  Options
	}{
		{name: "full", opt: Options{}},
		{name: "restricted", opt: Options{Restricted: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := listen(t, tc.opt)
			b := listen(t, tc.opt)
			if _, err := b.WriteTo([]byte("hello"), addrOf(t, a)); err != nil {
				t.Fatalf("write: %v", err)
			}
			ms := messages(1, 64)
			n, err := a.ReadBatch(ms, 0)
			if err != nil || n != 1 {
				t.Fatalf("read: %d %v", n, err)
			}
			if got := string(ms[0].Buffers[0][:ms[0].N]); got != "hello" {
				t.Fatalf("payload %q, want %q", got, "hello")
			}
			from, ok := ms[0].Addr.(*net.UDPAddr)
			if !ok || from.Port != addrOf(t, b).Port || !from.IP.Equal(net.IPv4(127, 0, 0, 1)) {
				t.Fatalf("source %v, want %v", ms[0].Addr, addrOf(t, b))
			}

			if _, err := a.WriteTo([]byte("world"), from); err != nil {
				t.Fatalf("reply: %v", err)
			}
			buf := make([]byte, 64)
			nr, _, err := b.ReadFrom(buf)
			if err != nil || string(buf[:nr]) != "world" {
				t.Fatalf("got %q %v, want %q", buf[:nr], err, "world")
			}
		})
	}
}

func TestDialWritesToConnectedSocket(t *testing.T) {
	a := listen(t, Options{})
	c, err := Dial(context.Background(), "udp4", addrOf(t, a), Options{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if c.RemoteAddr() == nil {
		t.Fatal("dialed socket has no remote address")
	}
	if _, err := c.WriteTo([]byte("ping"), addrOf(t, a)); err != nil {
		t.Fatalf("write to a connected socket: %v", err)
	}
	ms := messages(1, 64)
	if n, err := a.ReadBatch(ms, 0); err != nil || n != 1 {
		t.Fatalf("read: %d %v", n, err)
	}
	if _, err := a.WriteTo([]byte("pong"), addrOf(t, c)); err != nil {
		t.Fatalf("reply: %v", err)
	}
	buf := make([]byte, 64)
	n, from, err := c.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "pong" {
		t.Fatalf("read: %q %v", buf[:n], err)
	}
	if ua, ok := from.(*net.UDPAddr); !ok || ua.Port != addrOf(t, a).Port {
		t.Fatalf("source %v, want %v", from, addrOf(t, a))
	}

	// A batch read on a connected socket reads like a single one (quic-go never connects its
	// sockets, but the socket layer must not care).
	if _, err := a.WriteTo([]byte("pong2"), addrOf(t, c)); err != nil {
		t.Fatalf("reply: %v", err)
	}
	ms = messages(1, 64)
	if n, err := c.ReadBatch(ms, 0); err != nil || n != 1 || string(ms[0].Buffers[0][:ms[0].N]) != "pong2" {
		t.Fatalf("batch read on a connected socket: %d %v", n, err)
	}
}

func TestWriteBatchOnConnectedSocket(t *testing.T) {
	a := listen(t, Options{})
	c, err := Dial(context.Background(), "udp4", addrOf(t, a), Options{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	bufs := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	n, err := c.WriteBatch(addrOf(t, a), bufs)
	if err != nil || n != len(bufs) {
		t.Fatalf("batch write on a connected socket: %d %v", n, err)
	}
	ms := messages(8, 64)
	got := 0
	for got < len(bufs) {
		n, err := a.ReadBatch(ms, 0)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got += n
	}
}

func TestProtectRunsForEverySocket(t *testing.T) {
	var mu sync.Mutex
	var fds []uintptr
	protect := func(fd uintptr) error {
		mu.Lock()
		defer mu.Unlock()
		if fd == 0 {
			return errors.New("zero descriptor")
		}
		fds = append(fds, fd)
		return nil
	}

	a, err := Listen(context.Background(), "udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, Options{Protect: protect})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer a.Close()
	c, err := Dial(context.Background(), "udp4", addrOf(t, a), Options{Protect: protect})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	raw, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("raw socket: %v", err)
	}
	defer raw.Close()
	if _, err := Wrap(raw, Options{Protect: protect}); err != nil {
		t.Fatalf("wrap: %v", err)
	}

	mu.Lock()
	got := len(fds)
	mu.Unlock()
	if got != 3 {
		t.Fatalf("protect ran %d times, want 3", got)
	}
}

func TestProtectErrorAbortsSocket(t *testing.T) {
	boom := errors.New("protected")
	_, err := Listen(context.Background(), "udp4", nil, Options{Protect: func(uintptr) error { return boom }})
	if !errors.Is(err, boom) {
		t.Fatalf("listen with a failing protect: %v, want %v", err, boom)
	}
}

func TestTransportConn(t *testing.T) {
	full := listen(t, Options{})
	if _, ok := full.TransportConn().(*Conn); !ok {
		t.Fatalf("full mode hands %T to the transport, want *Conn", full.TransportConn())
	}

	restricted := listen(t, Options{Restricted: true})
	handed := restricted.TransportConn()
	if _, ok := handed.(*Conn); ok {
		t.Fatal("restricted mode hands the Conn to the transport, which turns the fast path on")
	}
	if _, ok := handed.(interface {
		ReadMsgUDP(b, oob []byte) (int, int, int, *net.UDPAddr, error)
	}); ok {
		t.Fatalf("restricted mode hands %T to the transport with its OOB methods", handed)
	}
	if _, ok := handed.(interface {
		SyscallConn() (syscall.RawConn, error)
	}); !ok {
		t.Fatalf("restricted mode hides SyscallConn: the transport cannot set DF or tune the buffers")
	}
	if restricted.ecnOn || restricted.gso.Load() || restricted.batchRead.Load() {
		t.Fatalf("restricted socket has ecn=%v gso=%v batch=%v, want all off",
			restricted.ecnOn, restricted.gso.Load(), restricted.batchRead.Load())
	}

	// A restricted socket never touches the batched calls, even when the caller uses Conn
	// directly instead of handing it to a transport.
	peer := listen(t, Options{})
	restricted.readKernelFn = func([]ipv4.Message, int) (int, error) {
		t.Error("restricted socket used a batch read")
		return 0, errors.New("unexpected")
	}
	restricted.writeMsgFn = func(p, oob []byte, addr *net.UDPAddr) (int, int, error) {
		if oob != nil {
			t.Error("restricted socket wrote with control messages")
		}
		return restricted.writeRaw(p, oob, addr)
	}
	if _, err := peer.WriteTo([]byte("x"), addrOf(t, restricted)); err != nil {
		t.Fatalf("write: %v", err)
	}
	ms := messages(1, 16)
	if n, err := restricted.ReadBatch(ms, 0); err != nil || n != 1 {
		t.Fatalf("restricted read: %d %v", n, err)
	}
	if _, err := restricted.WriteTo([]byte("y"), addrOf(t, peer)); err != nil {
		t.Fatalf("restricted write: %v", err)
	}
}

// runDatagrams sends the whole sequence through one socket pair and fails on the first datagram
// that is lost, duplicated or altered. Both paths produce the same verified sequence.
func runDatagrams(t *testing.T, opt Options, batched bool) {
	t.Helper()
	recv := listen(t, opt)
	send := listen(t, opt)
	if err := recv.SetReadBuffer(testRecvBuffer); err != nil {
		t.Fatalf("receive buffer: %v", err)
	}
	dst := addrOf(t, recv)

	sem := make(chan struct{}, recvWindow)
	for i := 0; i < recvWindow; i++ {
		sem <- struct{}{}
	}
	seen := make([]int, totalDatagrams)
	errCh := make(chan error, 1)
	go func() {
		ms := messages(maxWriteBatch, 2048)
		got := 0
		for got < totalDatagrams {
			if err := recv.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
				errCh <- err
				return
			}
			n, err := recv.ReadBatch(ms, 0)
			if err != nil {
				errCh <- fmt.Errorf("after %d datagrams: %w", got, err)
				return
			}
			for i := 0; i < n; i++ {
				idx, ok := checkPayload(ms[i].Buffers[0][:ms[i].N])
				if !ok {
					errCh <- fmt.Errorf("datagram %d: payload of %d bytes is not one of ours", got, ms[i].N)
					return
				}
				seen[idx]++
				got++
				sem <- struct{}{}
			}
		}
		errCh <- nil
	}()

	acquire := func() {
		select {
		case <-sem:
		case err := <-errCh:
			if err == nil {
				err = errors.New("receiver stopped early")
			}
			t.Fatalf("receive: %v", err)
		}
	}

	for i := 0; i < totalDatagrams; {
		end := i + 1
		for end < totalDatagrams && end-i < maxWriteBatch && len(allPayloads[end]) == len(allPayloads[i]) {
			end++
		}
		chunk := allPayloads[i:end]
		for range chunk {
			acquire()
		}
		if batched {
			n, err := send.WriteBatch(dst, chunk)
			if err != nil || n != len(chunk) {
				t.Fatalf("batch write [%d,%d): %d %v", i, end, n, err)
			}
		} else {
			for _, p := range chunk {
				if _, err := send.WriteTo(p, dst); err != nil {
					t.Fatalf("write %d: %v", i, err)
				}
			}
		}
		i = end
	}

	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	for i, n := range seen {
		if n != 1 {
			t.Fatalf("datagram %d delivered %d times, want once", i, n)
		}
	}
}

func TestTenThousandDatagramsSinglePath(t *testing.T) {
	runDatagrams(t, Options{Restricted: true}, false)
}

func TestTenThousandDatagramsBatchedPath(t *testing.T) {
	runDatagrams(t, Options{}, true)
}

func TestReadDeadlineIsNotABatchFailure(t *testing.T) {
	c := listen(t, Options{})
	if !c.batchRead.Load() {
		t.Skip("the platform has no batch read")
	}
	c.readKernelFn = func(ms []ipv4.Message, flags int) (int, error) {
		return c.readKernel(ms, flags)
	}
	if err := c.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	ms := messages(1, 16)
	if _, err := c.ReadBatch(ms, 0); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read: %v, want a deadline error", err)
	}
	if !c.batchRead.Load() {
		t.Fatal("batch read was disabled by a deadline, not by a refused call")
	}
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear deadline: %v", err)
	}
	peer := listen(t, Options{})
	if _, err := peer.WriteTo([]byte("late"), addrOf(t, c)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if n, err := c.ReadBatch(ms, 0); err != nil || n != 1 || string(ms[0].Buffers[0][:ms[0].N]) != "late" {
		t.Fatalf("read after the deadline: %d %v", n, err)
	}
}

// TestClosedSocketIsNotABatchFailure proves a closed socket keeps the batched flags as they are:
// a closed socket says nothing about the kernel's willingness to batch. The error itself must
// still reach the caller.
func TestClosedSocketIsNotABatchFailure(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		c := listen(t, Options{})
		if !c.batchRead.Load() {
			t.Skip("the platform has no batch read")
		}
		if err := c.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if _, err := c.ReadBatch(messages(1, 16), 0); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("read on a closed socket: %v, want net.ErrClosed", err)
		}
		if !c.batchRead.Load() {
			t.Fatal("batch read was disabled by a closed socket")
		}
	})

	t.Run("write", func(t *testing.T) {
		c := listen(t, Options{})
		if !c.gso.Load() {
			t.Skip("the platform has no GSO")
		}
		peer := listen(t, Options{})
		if err := c.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		bufs := [][]byte{bytes.Repeat([]byte{1}, 300), bytes.Repeat([]byte{2}, 300)}
		n, err := c.WriteBatch(addrOf(t, peer), bufs)
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("write on a closed socket: %v, want net.ErrClosed", err)
		}
		if n != 0 {
			t.Fatalf("%d datagrams reported as sent on a closed socket, want 0", n)
		}
		if !c.gso.Load() {
			t.Fatal("GSO was disabled by a closed socket")
		}
	})
}

func TestBatchFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline", fmt.Errorf("read: %w", os.ErrDeadlineExceeded), false},
		{"closed", fmt.Errorf("write: %w", net.ErrClosed), false},
		{"refused", errors.New("UDP_SEGMENT refused"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := batchFailure(tc.err); got != tc.want {
				t.Fatalf("batchFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestNoGSOWithoutTheFlag checks that a restricted socket never asks for a GSO train, and a full
// socket does at least try one on a kernel that offers it.
func TestSegmentable(t *testing.T) {
	buf := func(n int) []byte { return make([]byte, n) }
	cases := []struct {
		name string
		bufs [][]byte
		want int
	}{
		{"one datagram", [][]byte{buf(1200)}, 0},
		{"empty", nil, 0},
		{"equal", [][]byte{buf(1200), buf(1200), buf(1200)}, 1200},
		{"last shorter", [][]byte{buf(1200), buf(1200), buf(700)}, 1200},
		{"last zero", [][]byte{buf(1200), buf(0)}, 0},
		{"last longer", [][]byte{buf(1200), buf(1400)}, 0},
		{"middle differs", [][]byte{buf(1200), buf(900), buf(1200)}, 0},
		{"zero first", [][]byte{buf(0), buf(0)}, 0},
		{"jumbo", [][]byte{buf(65536), buf(65536)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := segmentable(tc.bufs); got != tc.want {
				t.Fatalf("segmentable = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNoGSOWithoutTheFlag(t *testing.T) {
	if !listen(t, Options{}).gso.Load() {
		t.Skip("the platform has no GSO")
	}
	restricted := listen(t, Options{Restricted: true})
	peer := listen(t, Options{})
	tried := atomic.Int32{}
	restricted.writeMsgFn = func(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
		tried.Add(1)
		return restricted.writeRaw(b, oob, addr)
	}
	bufs := [][]byte{bytes.Repeat([]byte{1}, 300), bytes.Repeat([]byte{2}, 300)}
	if _, err := restricted.WriteBatch(addrOf(t, peer), bufs); err != nil {
		t.Fatalf("write: %v", err)
	}
	if tried.Load() != 2 {
		t.Fatalf("restricted write touched the socket %d times, want 2 single writes", tried.Load())
	}
}
