package reality2

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
)

// blackholeConn is a real TCP socket behind a path with a small PMTU and no ICMP: what the client
// writes while its MSS is too large for the path never arrives (T3.12, the T15.0c polygon in the
// small). Writes after the limit is honoured pass.
type blackholeConn struct {
	net.Conn
	drop bool
}

func (c *blackholeConn) Write(p []byte) (int, error) {
	if c.drop {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

type mssLog struct {
	mu    sync.Mutex
	calls []int
}

func (l *mssLog) add(m int) { l.mu.Lock(); l.calls = append(l.calls, m); l.mu.Unlock() }
func (l *mssLog) get() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int(nil), l.calls...)
}

// TestTCPDialRetriesOnALimitedMSS: on a path of 1280 without ICMP the first attempt, with the
// system MSS, is swallowed; the retry with MSS 1240 connects, within 5 s, and the network is marked.
func TestTCPDialRetriesOnALimitedMSS(t *testing.T) {
	for _, mtu := range []int{1280, 1200} {
		r := newTCPRig(t)
		var log mssLog
		narrow := carrier.NewNarrowPath(nil, nil)
		narrow.FirstTry, narrow.Rung = 400*time.Millisecond, 400*time.Millisecond
		limit := mtu - 40 // the largest MSS the path carries
		req := carrier.DialRequest{
			Card: r.card, Addr: r.addr, Narrow: narrow, NetworkID: func() string { return "n1" },
			DialMSS: func(ctx context.Context, network, addr string, mss int) (net.Conn, error) {
				log.add(mss)
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				eff := mss
				if eff == 0 {
					eff = 1460
				}
				return &blackholeConn{Conn: c, drop: eff > limit}, nil
			},
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if s, err := r.ln.Accept(ctx); err == nil {
				s.Close()
			}
		}()
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := NewTCPDialer(TCPDialerConfig{}).Dial(ctx, req)
		cancel()
		if err != nil {
			t.Fatalf("mtu %d: Dial: %v (sockets %v)", mtu, err, log.get())
		}
		c.Close()
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("mtu %d: connected after %v", mtu, d)
		}
		got := log.get()
		if got[0] != 0 {
			t.Fatalf("mtu %d: the first socket carried MSS %d", mtu, got[0])
		}
		want := []int{0, 1240}
		if mtu == 1200 {
			want = []int{0, 1240, 1160}
		}
		if len(got) != len(want) || got[len(got)-1] != want[len(want)-1] {
			t.Fatalf("mtu %d: sockets %v, want %v", mtu, got, want)
		}
		if !narrow.Marked("n1") {
			t.Fatalf("mtu %d: the network was not marked", mtu)
		}
	}
}

// TestQUICInitialFitsA1280Path: the reality2-quic Initial must fit PMTU 1280: at most 1252 bytes of
// UDP payload over IPv4 (1252 + 28 = 1280) and at most 1232 over IPv6 (1232 + 48 = 1280), and at
// least 1200 (RFC 9000 section 14.1). The quic-go sizes are 1250 and 1232 (T3.12).
func TestQUICInitialFitsA1280Path(t *testing.T) {
	for _, tc := range []struct {
		name, ip string
		max      int
	}{{"IPv4", "127.0.0.1", 1280 - 28}, {"IPv6", "::1", 1280 - 48}} {
		t.Run(tc.name, func(t *testing.T) {
			ua, err := net.ResolveUDPAddr("udp", net.JoinHostPort(tc.ip, "0"))
			if err != nil {
				t.Skip(err)
			}
			sink, err := net.ListenUDP("udp", ua)
			if err != nil {
				t.Skip("no " + tc.name + " loopback: " + err.Error())
			}
			defer sink.Close()
			port := uint32(sink.LocalAddr().(*net.UDPAddr).Port)
			sk := mustKey(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			go func() {
				_, _ = NewDialer(DialerConfig{}).Dial(ctx, carrier.DialRequest{
					Card: testCard(t, sk), Addr: testAddrPB(t, tc.ip, port, "cover.example")})
			}()
			buf := make([]byte, 4096)
			_ = sink.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, _, err := sink.ReadFromUDP(buf)
			if err != nil {
				t.Fatal(err)
			}
			if n < 1200 || n > tc.max {
				t.Fatalf("first datagram is %d bytes, want 1200..%d", n, tc.max)
			}
		})
	}
}
