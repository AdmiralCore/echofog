package congestion

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shapedConn is a PacketConn whose sends cross an emulated bottleneck: a fixed rate, a queue of
// bounded size (drop-tail, like a home router), a one-way delay and random loss. It is what
// tc netem does, in-process, so it also runs on Windows.
type shapedConn struct {
	net.PacketConn
	rate  float64 // bytes per second
	delay time.Duration
	loss  float64
	limit time.Duration // the queue holds this much sending time

	mu   sync.Mutex
	free time.Time // when the bottleneck is idle again
	rnd  *mrand.Rand
	line chan shaped // in order, like a real link: reordering would read as loss
	done chan struct{}
	once sync.Once
}

type shaped struct {
	b    []byte
	addr net.Addr
	at   time.Time
}

func shape(c net.PacketConn, mbps float64, delay time.Duration, loss float64) *shapedConn {
	s := &shapedConn{PacketConn: c, rate: mbps * 1e6 / 8, delay: delay, loss: loss,
		limit: 2*delay + 20*time.Millisecond, rnd: mrand.New(mrand.NewPCG(1, 2)), line: make(chan shaped, 1<<16), done: make(chan struct{})}
	go func() {
		for {
			select {
			case p := <-s.line:
				if d := time.Until(p.at); d > 0 {
					time.Sleep(d)
				}
				_, _ = s.PacketConn.WriteTo(p.b, p.addr)
			case <-s.done:
				return
			}
		}
	}()
	return s
}

func (s *shapedConn) Close() error {
	s.once.Do(func() { close(s.done) })
	return s.PacketConn.Close()
}

func (s *shapedConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	now := time.Now()
	s.mu.Lock()
	if s.free.Before(now) {
		s.free = now
	}
	if s.free.Sub(now) > s.limit || s.rnd.Float64() < s.loss {
		s.mu.Unlock()
		return len(p), nil // queue full or lost on the way
	}
	s.free = s.free.Add(time.Duration(float64(len(p)) / s.rate * float64(time.Second)))
	at := s.free.Add(s.delay)
	select {
	case s.line <- shaped{append([]byte(nil), p...), addr, at}:
	default: // the emulator itself is full
	}
	s.mu.Unlock()
	return len(p), nil
}

func testTLS(t testing.TB) (*tls.Config, *tls.Config) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"test"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	srv := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"t"}}
	return srv, &tls.Config{InsecureSkipVerify: true, ServerName: "test", NextProtos: []string{"t"}}
}

// download moves bytes from a server to a client across the shaped path for d and returns
// Mbit/s. The server sends, so its controller is the one under test.
func download(t testing.TB, c Config, mbps float64, rtt time.Duration, loss float64, d time.Duration) float64 {
	srvTLS, cliTLS := testTLS(t)
	cfg := &quic.Config{MaxIdleTimeout: 10 * time.Second, InitialStreamReceiveWindow: 8 << 20,
		MaxStreamReceiveWindow: 32 << 20, InitialConnectionReceiveWindow: 16 << 20, MaxConnectionReceiveWindow: 48 << 20}

	sc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer sc.Close()
	cc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer cc.Close()
	srvTr := &quic.Transport{Conn: shape(sc, mbps, rtt/2, loss)}
	cliTr := &quic.Transport{Conn: shape(cc, mbps, rtt/2, loss)}
	defer srvTr.Close()
	defer cliTr.Close()

	ln, err := srvTr.Listen(srvTLS, cfg)
	require.NoError(t, err)
	go func() {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			return
		}
		Apply(conn, c)
		st, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		buf := make([]byte, 64<<10)
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if _, err := st.Write(buf); err != nil {
				return
			}
		}
		st.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), d+15*time.Second)
	defer cancel()
	conn, err := cliTr.Dial(ctx, sc.LocalAddr(), cliTLS, cfg)
	require.NoError(t, err)
	defer conn.CloseWithError(0, "")
	st, err := conn.OpenStreamSync(ctx)
	require.NoError(t, err)
	_, err = st.Write([]byte{1})
	require.NoError(t, err)
	start := time.Now()
	n, _ := io.Copy(io.Discard, st)
	return float64(n) * 8 / 1e6 / time.Since(start).Seconds()
}

func TestParse(t *testing.T) {
	c, err := Parse("", 0)
	require.NoError(t, err)
	assert.Equal(t, BBR, c.Mode, "BBR is the default")
	c, err = Parse("Cubic", 0)
	require.NoError(t, err)
	assert.Equal(t, Cubic, c.Mode)
	c, err = Parse("brutal", 80)
	require.NoError(t, err)
	assert.Equal(t, Config{Mode: Brutal, BrutalBps: 10_000_000}, c)
	_, err = Parse("brutal", 0)
	assert.Error(t, err, "brutal without a rate would send at nothing or everything")
	_, err = Parse("vegas", 0)
	assert.Error(t, err)

	t.Setenv(EnvOverride, "brutal:8")
	assert.Equal(t, Config{Mode: Brutal, BrutalBps: 1_000_000}, FromEnv(Config{Mode: BBR}))
	t.Setenv(EnvOverride, "nonsense")
	assert.Equal(t, Config{Mode: BBR}, FromEnv(Config{Mode: BBR}), "a bad override is ignored")
}

// BBR on a lossy long link: data arrives intact and far faster than the loss would let Cubic.
// The comparison itself is in TestLossyLinkThroughput; this one only keeps BBR working.
func TestBBROverALossyLink(t *testing.T) {
	got := download(t, Config{Mode: BBR}, 40, 60*time.Millisecond, 0.01, 3*time.Second)
	t.Logf("BBR, 40 Mbit/s, 60 ms, 1%% loss: %.1f Mbit/s", got)
	assert.Greater(t, got, 8.0, "BBR must keep a good share of the line through 1% loss")
}

// TestLossyLinkThroughput compares the controllers across links like those of filtered networks.
//
//	ECHOFOG_BENCH=1 go test ./internal/network/congestion -run TestLossyLinkThroughput -v
func TestLossyLinkThroughput(t *testing.T) {
	if os.Getenv("ECHOFOG_BENCH") == "" {
		t.Skip("set ECHOFOG_BENCH=1 to measure")
	}
	links := []struct {
		mbps float64
		rtt  time.Duration
		loss float64
	}{
		{100, 50 * time.Millisecond, 0},
		{100, 150 * time.Millisecond, 0.01},
		{100, 150 * time.Millisecond, 0.03},
		{50, 250 * time.Millisecond, 0.02},
	}
	for _, l := range links {
		cubic := download(t, Config{Mode: Cubic}, l.mbps, l.rtt, l.loss, 8*time.Second)
		bbrRate := download(t, Config{Mode: BBR}, l.mbps, l.rtt, l.loss, 8*time.Second)
		brutalRate := download(t, Config{Mode: Brutal, BrutalBps: uint64(l.mbps * 0.9e6 / 8)}, l.mbps, l.rtt, l.loss, 8*time.Second)
		t.Logf("%3.0f Mbit/s %4v %3.0f%% loss:  cubic %6.1f   bbr %6.1f   brutal(90%%) %6.1f Mbit/s",
			l.mbps, l.rtt, l.loss*100, cubic, bbrRate, brutalRate)
	}
}
