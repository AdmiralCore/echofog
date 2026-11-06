package exit

import (
	"bytes"
	"context"
	"errors"
	"io"
	mrand "math/rand/v2"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// clientRig is a Client wired to the rig's server through a real session.
type clientRig struct {
	*rig
	cl *Client
}

func newClientRig(t *testing.T, rc rigConfig) *clientRig {
	t.Helper()
	return newClientRigWith(t, rc, nil, func(time.Duration) {})
}

func newClientRigWith(t *testing.T, rc rigConfig, prof *shape.Profile, sleep func(time.Duration)) *clientRig {
	t.Helper()
	r := newRig(t, rc)
	cfg := ClientConfig{
		OpenStream: func(ctx context.Context) (*mux.Stream, error) {
			st, err := r.client.OpenStream(ctx, 6, 2)
			if err != nil {
				return nil, err
			}
			st.SetLimiter(limiter())
			return st, nil
		},
		Shape: prof,
		Sleep: sleep,
	}
	if !rc.NoDatagrams {
		cfg.SendDatagram = r.client.SendDatagram
	}
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	go func() {
		for {
			select {
			case d := <-r.dgrams:
				if d.Type == frame.DgramExitUDP {
					cl.HandleDatagram(d.Payload)
				}
			case <-r.ctx.Done():
				return
			}
		}
	}()
	return &clientRig{rig: r, cl: cl}
}

func tcpEcho(t *testing.T, prefix string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						_, _ = c.Write(append([]byte(prefix), buf[:n]...))
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func udpEcho(t *testing.T, reply []byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			out := reply
			if out == nil {
				out = append([]byte(nil), buf[:n]...)
			}
			_, _ = pc.WriteTo(out, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func readFull(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(r, buf); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read timed out")
	}
	return buf
}

func TestClientTCPEcho(t *testing.T) {
	target := tcpEcho(t, "echo:")
	cr := newClientRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := cr.cl.DialExitStream(ctx, target)
	if err != nil {
		t.Fatalf("DialExitStream: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readFull(t, conn, len("echo:hi")); string(got) != "echo:hi" {
		t.Fatalf("got %q", got)
	}
}

// A body bigger than one DATA frame (16384) crosses the tunnel whole and in order.
func TestClientTCPBulk(t *testing.T) {
	target := tcpEcho(t, "")
	cr := newClientRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := cr.cl.DialExitStream(ctx, target)
	if err != nil {
		t.Fatalf("DialExitStream: %v", err)
	}
	defer conn.Close()
	big := make([]byte, 100_000)
	for i := range big {
		big[i] = byte(i * 7)
	}
	go func() { _, _ = conn.Write(big) }()
	if got := readFull(t, conn, len(big)); !bytes.Equal(got, big) {
		t.Fatal("bulk echo differs")
	}
}

func TestClientTCPRefusals(t *testing.T) {
	tests := []struct {
		name   string
		cfg    ServerConfig
		target string
		want   error
	}{
		{"private target", ServerConfig{}, "127.0.0.1:9", core.ErrExitDenied},
		{"closed port", ServerConfig{AllowPrivateTargets: true}, "127.0.0.1:1", core.ErrExitDialFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cr := newClientRig(t, rigConfig{ServerConfig: tc.cfg})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := cr.cl.DialExitStream(ctx, tc.target)
			if err == nil {
				defer conn.Close()
				_, err = conn.Read(make([]byte, 1))
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestClientRejectsBadTargets(t *testing.T) {
	cr := newClientRig(t, rigConfig{})
	ctx := context.Background()
	for _, target := range []string{
		"", "nohost", "host:", ":80", "host:0", "host:65536", "host:abc",
		strings.Repeat("a", 254) + ":80",
	} {
		if conn, err := cr.cl.DialExitStream(ctx, target); err == nil {
			_ = conn.Close()
			t.Errorf("target %q accepted", target)
		}
	}
	if n := cr.srv.Stats().Accepted.Load() + cr.srv.Stats().Violations.Load(); n != 0 {
		t.Fatalf("a bad target reached the exit (%d)", n)
	}
}

func TestClientUDPDatagrams(t *testing.T) {
	big := bytes.Repeat([]byte("back"), 800)
	target := udpEcho(t, big)
	cr := newClientRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, err := cr.cl.DialExitUDP(ctx)
	if err != nil {
		t.Fatalf("DialExitUDP: %v", err)
	}
	defer u.Close()
	if !u.Datagrams() {
		t.Fatal("UDP does not ride EXIT_UDP datagrams although the session carries them")
	}
	// The request needs several fragments, the reply too.
	if err := u.WriteTo(bytes.Repeat([]byte("q"), 3000), target); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	data, addr := readUDP(t, u)
	if addr != target || !bytes.Equal(data, big) {
		t.Fatalf("reply from %q, %d bytes", addr, len(data))
	}
}

func TestClientUDPStreamFallback(t *testing.T) {
	target := udpEcho(t, nil)
	cr := newClientRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true}, NoDatagrams: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, err := cr.cl.DialExitUDP(ctx)
	if err != nil {
		t.Fatalf("DialExitUDP: %v", err)
	}
	defer u.Close()
	if u.Datagrams() {
		t.Fatal("datagram mode without a datagram sender")
	}
	if err := u.WriteTo([]byte("ping"), target); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	data, addr := readUDP(t, u)
	if addr != target || string(data) != "ping" {
		t.Fatalf("reply %q from %q", data, addr)
	}
}

func readUDP(t *testing.T, u *core.UDPSession) ([]byte, string) {
	t.Helper()
	type res struct {
		d []byte
		a string
		e error
	}
	ch := make(chan res, 1)
	go func() { d, a, e := u.ReadFrom(); ch <- res{d, a, e} }()
	select {
	case r := <-ch:
		if r.e != nil {
			t.Fatalf("ReadFrom: %v", r.e)
		}
		return r.d, r.a
	case <-time.After(5 * time.Second):
		t.Fatal("ReadFrom timed out")
	}
	return nil, ""
}

func TestClientUDPLimitRefusal(t *testing.T) {
	cr := newClientRig(t, rigConfig{ServerConfig: ServerConfig{Limits: Limits{UDPSessions: 1}, AllowPrivateTargets: true}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, err := cr.cl.DialExitUDP(ctx)
	if err != nil {
		t.Fatalf("first DialExitUDP: %v", err)
	}
	defer u.Close()
	if _, err := cr.cl.DialExitUDP(ctx); !errors.Is(err, core.ErrExitBusy) {
		t.Fatalf("second DialExitUDP error = %v, want busy", err)
	}
}

func TestClientHandleDatagramHostile(t *testing.T) {
	cr := newClientRig(t, rigConfig{})
	for _, p := range [][]byte{nil, {}, {0xff}, {0, 0, 0, 0, 0, 1, 8, 9}, bytes.Repeat([]byte{0xff}, 2000),
		Fragment{Session: 99, Packet: 1, Count: 1, Addr: "a:1", Data: []byte("x")}.Append(nil)} {
		cr.cl.HandleDatagram(p) // no session: dropped, no panic
	}
}

// recordedStream is a wireStream that records what the client writes.
type recordedStream struct {
	mu     sync.Mutex
	frames []frame.Frame
	in     chan frame.Frame
}

func newRecordedStream() *recordedStream { return &recordedStream{in: make(chan frame.Frame, 8)} }

func (r *recordedStream) WriteFrame(typ uint64, flags byte, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, frame.Frame{Type: typ, Flags: flags, Payload: append([]byte(nil), payload...)})
	return nil
}

func (r *recordedStream) ReadFrame() (frame.Frame, error) {
	f, ok := <-r.in
	if !ok {
		return frame.Frame{}, io.EOF
	}
	return f, nil
}
func (r *recordedStream) CloseWrite() error { return nil }
func (r *recordedStream) Close() error      { return nil }

func (r *recordedStream) snapshot() []frame.Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]frame.Frame(nil), r.frames...)
}

// wireSizes returns the on-wire size of each DATA record with the PADDING that precedes it.
func wireSizes(fs []frame.Frame) []int {
	var out []int
	pad := 0
	for _, f := range fs {
		switch f.Type {
		case frame.TypePadding:
			pad += frame.FrameLen(f.Type, len(f.Payload))
		case TypeData:
			out = append(out, pad+frame.FrameLen(f.Type, len(f.Payload)))
			pad = 0
		}
	}
	return out
}

// H1: the first records of the client's direction are padded to the exit address profile, and
// only the first 8 records or 16 KiB.
func TestClientPadsFirstRecords(t *testing.T) {
	prof, err := shape.Builtin(shape.IDDefaultTLS)
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	rec := newRecordedStream()
	cs := newClientStream(rec, prof, func(d time.Duration) { slept = append(slept, d) }, mrand.New(mrand.NewPCG(1, 2)))
	for i := 0; i < 12; i++ {
		if _, err := cs.Write(bytes.Repeat([]byte{'x'}, 300)); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	sizes := wireSizes(rec.snapshot())
	if len(sizes) != 12 {
		t.Fatalf("%d DATA records, want 12", len(sizes))
	}
	buckets := map[int]bool{}
	for _, b := range prof.Buckets {
		buckets[int(b)] = true
	}
	for i, n := range sizes[:padRecords] {
		if !buckets[n] {
			t.Errorf("record %d is %d bytes on the wire, not a profile bucket %v", i, n, prof.Buckets)
		}
	}
	for i, n := range sizes[padRecords:] {
		if want := frame.FrameLen(TypeData, 300); n != want {
			t.Errorf("record %d after the padded prefix is %d bytes, want unpadded %d", padRecords+i, n, want)
		}
	}
	for _, d := range slept {
		if d < 0 || d > maxJitter {
			t.Errorf("jitter %v out of [0, %v]", d, maxJitter)
		}
	}
	if len(slept) == 0 {
		t.Error("no jitter before padded records")
	}
}

func TestClientNoPaddingWithoutProfile(t *testing.T) {
	rec := newRecordedStream()
	cs := newClientStream(rec, nil, func(time.Duration) { t.Error("slept without a profile") }, nil)
	if _, err := cs.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	fs := rec.snapshot()
	if len(fs) != 1 || fs[0].Type != TypeData {
		t.Fatalf("frames = %v, want one DATA", fs)
	}
}

// The wire sizes of the first records must not reproduce the sizes of the inner TLS handshake.
func TestClientFirstRecordSizesDifferFromInner(t *testing.T) {
	prof, err := shape.Builtin(shape.IDDefaultUDP)
	if err != nil {
		t.Fatal(err)
	}
	inner := []int{517, 1460, 93, 51} // ClientHello, ServerHello+certs, Finished-sized writes
	distinct := map[int]bool{}
	same, total := 0, 0
	for run := 0; run < 200; run++ {
		rec := newRecordedStream()
		cs := newClientStream(rec, prof, func(time.Duration) {}, mrand.New(mrand.NewPCG(uint64(run), 7)))
		for _, n := range inner {
			if _, err := cs.Write(bytes.Repeat([]byte{'t'}, n)); err != nil {
				t.Fatal(err)
			}
		}
		for i, n := range wireSizes(rec.snapshot()) {
			total++
			distinct[n] = true
			if n == frame.FrameLen(TypeData, inner[i]) {
				same++
			}
		}
	}
	if total == 0 {
		t.Fatal("nothing written")
	}
	if len(distinct) < 20 {
		t.Errorf("only %d distinct wire sizes over %d records", len(distinct), total)
	}
	if same*10 > total {
		t.Errorf("%d of %d first records kept the inner size", same, total)
	}
}
