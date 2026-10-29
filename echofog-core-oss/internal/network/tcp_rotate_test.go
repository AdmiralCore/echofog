package network

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// freezeProxy stands between a client and a server and does what the TSPU does: after limit bytes
// of the server's answer on one connection it forwards nothing more, and closes nothing.
func freezeProxy(t *testing.T, target string, limit int, most *atomic.Int64) (addr string, frozen *atomic.Int64) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	frozen = new(atomic.Int64)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				s, err := net.Dial("tcp", target)
				if err != nil {
					c.Close()
					return
				}
				go func() { _, _ = io.Copy(s, c); s.Close() }()
				sent := 0
				buf := make([]byte, 4096)
				for {
					n, err := s.Read(buf)
					if n > 0 {
						if sent+n > limit {
							frozen.Add(1)
							for { // swallow the rest, as the censor does
								if _, err := s.Read(buf); err != nil {
									return
								}
							}
						}
						sent += n
						if most != nil && int64(sent) > most.Load() {
							most.Store(int64(sent))
						}
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						c.Close()
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), frozen
}

func echoStream(t *testing.T, out, in MuxConn, data []byte) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		st, err := in.AcceptStream(ctx)
		if err != nil {
			return
		}
		_, _ = io.Copy(st, st)
	}()
	st, err := out.OpenStreamSync(ctx)
	require.NoError(t, err)
	go func() { _, _ = st.Write(data) }()
	got := make([]byte, len(data))
	_ = st.SetReadDeadline(time.Now().Add(15 * time.Second))
	n, _ := io.ReadFull(st, got)
	return got[:n]
}

func TestTCPRotation_SurvivesTheFreeze(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server, client := newTCP(t, ss, ""), newTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	var most atomic.Int64
	proxy, frozen := freezeProxy(t, server.Addr().String(), 16<<10, &most)
	client.SetRotatePolicy(func(string) bool { return true })

	require.NoError(t, client.Dial(context.Background(), proxy))
	out, in := nextConn(t, client.ConnChan()), nextConn(t, server.ConnChan())
	data := make([]byte, 512<<10)
	_, _ = rand.Read(data)
	got := echoStream(t, out.Conn, in.Conn, data)
	require.True(t, bytes.Equal(got, data), "got %d of %d bytes", len(got), len(data))
	require.Zero(t, frozen.Load(), "a carrier went past the freeze count")
	t.Logf("most the server sent on one carrier: %d bytes", most.Load())
	require.LessOrEqual(t, most.Load(), int64(RotBudget+512), "a carrier went past its budget")
}

func TestTCPRotation_PlainLinkFreezes(t *testing.T) {
	ss, cs := newTestSigner(t), newTestSigner(t)
	server, client := newTCP(t, ss, ""), newTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	proxy, frozen := freezeProxy(t, server.Addr().String(), 16<<10, nil)

	require.NoError(t, client.Dial(context.Background(), proxy))
	out, in := nextConn(t, client.ConnChan()), nextConn(t, server.ConnChan())
	data := make([]byte, 512<<10)
	_, _ = rand.Read(data)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go func() {
		st, err := in.Conn.AcceptStream(ctx)
		if err == nil {
			_, _ = io.Copy(st, st)
		}
	}()
	st, err := out.Conn.OpenStreamSync(ctx)
	require.NoError(t, err)
	go func() { _, _ = st.Write(data) }()
	got := make([]byte, len(data))
	_ = st.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _ := io.ReadFull(st, got)
	require.Less(t, n, len(data), "the control: without rotation the freeze must stop the transfer")
	require.NotZero(t, frozen.Load())
}

func TestTCPRotation_FrozenAddressRotatesNext(t *testing.T) {
	tr := newTCP(t, newTestSigner(t), "")
	require.False(t, tr.shouldRotate("1.2.3.4:5"))
	tr.markFrozen("1.2.3.4:5")
	require.True(t, tr.shouldRotate("1.2.3.4:5"))
	require.False(t, tr.shouldRotate("1.2.3.4:6"))
	tr.SetRotatePolicy(func(a string) bool { return a == "9.9.9.9:1" })
	require.True(t, tr.shouldRotate("9.9.9.9:1"))
}

func TestTCPRotation_OrdinaryClientStillWorks(t *testing.T) {
	// a server that supports rotation keeps serving a client that does not ask for it (and the
	// other way round is TestTCPTransportConnectsPeers, which has no policy set)
	ss, cs := newTestSigner(t), newTestSigner(t)
	server, client := newTCP(t, ss, ""), newTCP(t, cs, "")
	require.NoError(t, server.Listen("127.0.0.1:0"))
	require.NoError(t, client.Dial(context.Background(), server.Addr().String()))
	out, in := nextConn(t, client.ConnChan()), nextConn(t, server.ConnChan())
	data := make([]byte, 64<<10)
	_, _ = rand.Read(data)
	require.True(t, bytes.Equal(echoStream(t, out.Conn, in.Conn, data), data))
}

// Where a TCP link has to rotate, the preference for TCP over QUIC is short: QUIC is the cheaper
// road and is tried again after two minutes, not ten.
func TestTCPRotation_ShortensThePreferenceForTCP(t *testing.T) {
	tr := newTCP(t, newTestSigner(t), "")
	f := &FallbackTransport{TCP: tr, mode: ModeAuto, preferT: map[string]time.Time{}, logger: slog.Default()}
	f.preferTCP("1.1.1.1:1")
	plain := f.preferT["1.1.1.1:1"]
	tr.SetRotatePolicy(func(string) bool { return true })
	f.preferTCP("2.2.2.2:2")
	rotating := f.preferT["2.2.2.2:2"]
	require.Greater(t, time.Until(plain), 9*time.Minute)
	require.Less(t, time.Until(rotating), 3*time.Minute)
}
