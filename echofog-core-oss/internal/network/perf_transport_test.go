package network

// Performance of every transport on loopback, both ends in this process:
//
//	ECHOFOG_PERF=1 go test ./internal/network -run TestPerfTransports -v -count=1 -timeout 30m
//
// ECHOFOG_PERF_MODES=plain,tcp picks modes (default all), ECHOFOG_PERF_MB the bulk size (64),
// ECHOFOG_PERF_REPEAT the repeats (5), ECHOFOG_PERF_PROFILE=dir writes CPU profiles of the bulk
// and paced phases (labelled side=client / side=server), ECHOFOG_PERF_OUT=file the results.
//
// Modes: quicgo (bare quic-go on a *net.UDPConn, the same quic.Config and BBR: what the stack
// costs on top of QUIC itself), plain (our transport, no obfuscation), direct (DirectSocket, no
// STUN multiplexer), noise (per-node UDP obfuscation), reality (QUIC-REALITY to a camouflage
// node), tcp (TLS mimicry + smux), ws (WebSocket + inner TLS + vision) and tls (bare TLS 1.3 on
// TCP, one connection per stream).

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/congestion"
	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/internal/testutil/perf"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

const perfSNI = "cover.lab"

var perfModes = []string{"quicgo", "plain", "direct", "noise", "reality", "tcp", "ws", "tls"}

// perfStream is the part of a stream the measurements use.
type perfStream interface {
	io.ReadWriter
	CloseWrite() error
	Close() error
}

// perfPair is a connected client and server of one mode.
type perfPair struct {
	open   func(ctx context.Context) (perfStream, error)
	accept func(ctx context.Context) (perfStream, error)
	close  func()
}

var (
	clientLabels = pprof.Labels("side", "client")
	serverLabels = pprof.Labels("side", "server")
)

// asSide runs f with the pprof labels of a side: goroutines it starts inherit them.
func asSide(labels pprof.LabelSet, f func()) {
	pprof.Do(context.Background(), labels, func(context.Context) { f() })
}

type perfQuicNode struct {
	tr *QuicTransport
	id types.PeerID
}

func perfNode(t *testing.T, obfs, direct bool) perfQuicNode {
	t.Helper()
	s := newTestSigner(t)
	tr, err := NewQUICTransportWithOptions("127.0.0.1:0", perfTLS(t), GetQuicConfig(), s, config.CurrentProtocolVersion,
		TransportOptions{Obfs: obfs, OwnID: s.id(), DirectSocket: direct})
	require.NoError(t, err)
	t.Cleanup(func() { tr.Close() })
	return perfQuicNode{tr: tr, id: s.id()}
}

// perfTLS is the node identity TLS config of testutil.GetRandomKeyAndTls.
func perfTLS(t *testing.T) *tls.Config {
	_, tlsCfg := testutil.GetRandomKeyAndTls(t)
	return tlsCfg
}

func perfNextConn(t *testing.T, ch <-chan NewConnEvent, d time.Duration) (NewConnEvent, bool) {
	select {
	case ev := <-ch:
		return ev, true
	case <-time.After(d):
		return NewConnEvent{}, false
	}
}

// perfCover is the site a camouflage node stands in front of.
func perfCover(t *testing.T) string {
	cert, err := mimicry.GenerateCamouflageCert(perfSNI)
	require.NoError(t, err)
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	srv := &http3.Server{
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "cover") }),
		TLSConfig: http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}),
	}
	go func() { _ = srv.Serve(udp) }()
	t.Cleanup(func() { _ = srv.Close(); _ = udp.Close() })
	return udp.LocalAddr().String()
}

// perfServer starts the server of a mode; dial connects a client to addr and returns its side of
// the connection. via, when set, wraps the server address (a delaying path) before dialing.
type perfServer struct {
	addr   string
	events <-chan NewConnEvent
	// dial makes a fresh client and connects it to addr; the client's connection is returned.
	dial func(ctx context.Context, addr string) (MuxConn, func(), error)
	udp  bool
}

func perfStartServer(t *testing.T, mode string) perfServer {
	t.Helper()
	switch mode {
	case "plain", "direct", "noise", "reality":
		var srv perfQuicNode
		asSide(serverLabels, func() {
			if mode == "reality" {
				cover := perfCover(t)
				srv = perfNode(t, true, false)
				cert, err := mimicry.GenerateCamouflageCert(perfSNI)
				require.NoError(t, err)
				srv.tr.SetCamouflage(cert, perfSNI, cover)
			} else {
				srv = perfNode(t, mode == "noise", mode == "direct")
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			require.NoError(t, srv.tr.ListenEarly(ctx))
		})
		return perfServer{addr: srv.tr.Addr().String(), events: srv.tr.ConnChan(), udp: true,
			dial: func(ctx context.Context, addr string) (MuxConn, func(), error) {
				var cli perfQuicNode
				var err error
				asSide(clientLabels, func() {
					cli = perfNode(t, mode == "noise" || mode == "reality", mode == "direct")
					if mode == "reality" {
						cli.tr.ExpectPeerSNI(addr, srv.id, perfSNI)
					} else {
						cli.tr.ExpectPeer(addr, srv.id)
					}
					err = cli.tr.DialEarly(ctx, addr)
				})
				if err != nil {
					cli.tr.Close()
					return nil, nil, err
				}
				ev, ok := perfNextConn(t, cli.tr.ConnChan(), 10*time.Second)
				if !ok {
					cli.tr.Close()
					return nil, nil, fmt.Errorf("no connection event")
				}
				return ev.Conn, func() { cli.tr.Close() }, nil
			}}
	case "tcp":
		ss := newTestSigner(t)
		var srv *TCPTransport
		asSide(serverLabels, func() {
			srv = newTCP(t, ss, "")
			srv.SetNodeKeys("", ss.id())
			require.NoError(t, srv.Listen("127.0.0.1:0"))
		})
		return perfServer{addr: srv.Addr().String(), events: srv.ConnChan(),
			dial: func(ctx context.Context, addr string) (MuxConn, func(), error) {
				var cli *TCPTransport
				var err error
				asSide(clientLabels, func() {
					cs := newTestSigner(t)
					cli = NewTCPTransport(testCert(t), testMimicry(""), NewDefaultPeerAuthenticator(cs, 101, 5*time.Second))
					cli.SetNodeKeys("", cs.id())
					cli.ExpectPeer(addr, ss.id())
					err = cli.Dial(ctx, addr)
				})
				if err != nil {
					cli.Close()
					return nil, nil, err
				}
				ev, ok := perfNextConn(t, cli.ConnChan(), 10*time.Second)
				if !ok {
					cli.Close()
					return nil, nil, fmt.Errorf("no connection event")
				}
				return ev.Conn, func() { cli.Close() }, nil
			}}
	case "ws":
		ss := newTestSigner(t)
		var srv *WSTransport
		asSide(serverLabels, func() {
			srv = newWS(t, ss, "/echofog", true)
			require.NoError(t, srv.Listen("127.0.0.1:0"))
		})
		return perfServer{addr: srv.Addr().String(), events: srv.ConnChan(),
			dial: func(ctx context.Context, addr string) (MuxConn, func(), error) {
				var cli *WSTransport
				var err error
				url := "ws://" + addr + "/echofog"
				asSide(clientLabels, func() {
					cs := newTestSigner(t)
					cli = NewWSTransport(testCert(t), testMimicry(""), NewDefaultPeerAuthenticator(cs, 101, 5*time.Second), "/echofog")
					cli.SetNodeKeys("", cs.id())
					cli.ExpectPeer(url, ss.id())
					err = cli.Dial(ctx, url)
				})
				if err != nil {
					cli.Close()
					return nil, nil, err
				}
				ev, ok := perfNextConn(t, cli.ConnChan(), 10*time.Second)
				if !ok {
					cli.Close()
					return nil, nil, fmt.Errorf("no connection event")
				}
				return ev.Conn, func() { cli.Close() }, nil
			}}
	}
	t.Fatalf("unknown mode %s", mode)
	return perfServer{}
}

// perfMuxPair connects one client of an echofog transport and serves streams on the server side.
func perfMuxPair(t *testing.T, mode string) perfPair {
	srv := perfStartServer(t, mode)
	cli, closeCli, err := srv.dial(context.Background(), srv.addr)
	require.NoError(t, err)
	in, ok := perfNextConn(t, srv.events, 10*time.Second)
	require.True(t, ok, "server saw no connection")
	return perfPair{
		open:   func(ctx context.Context) (perfStream, error) { return cli.OpenStreamSync(ctx) },
		accept: func(ctx context.Context) (perfStream, error) { return in.Conn.AcceptStream(ctx) },
		close:  func() { closeCli(); _ = in.Conn.CloseWithError(0, "") },
	}
}

// perfQuicStream gives a bare quic-go stream TCP's half-close.
type perfQuicStream struct{ *quic.Stream }

func (s perfQuicStream) CloseWrite() error { return s.Stream.Close() }
func (s perfQuicStream) Close() error {
	s.Stream.CancelRead(0)
	return s.Stream.Close()
}

func perfQuicGoPair(t *testing.T) perfPair {
	cc := congestion.Config{}
	var ln *quic.Listener
	asSide(serverLabels, func() {
		var err error
		ln, err = quic.ListenAddr("127.0.0.1:0", perfTLS(t), GetQuicConfig())
		require.NoError(t, err)
	})
	t.Cleanup(func() { ln.Close() })
	var cli *quic.Conn
	asSide(clientLabels, func() {
		var err error
		cli, err = quic.DialAddr(context.Background(), ln.Addr().String(), perfTLS(t), GetQuicConfig())
		require.NoError(t, err)
		congestion.Apply(cli, cc)
	})
	srv, err := ln.Accept(context.Background())
	require.NoError(t, err)
	congestion.Apply(srv, cc)
	return perfPair{
		open: func(ctx context.Context) (perfStream, error) {
			s, err := cli.OpenStreamSync(ctx)
			return perfQuicStream{s}, err
		},
		accept: func(ctx context.Context) (perfStream, error) {
			s, err := srv.AcceptStream(ctx)
			return perfQuicStream{s}, err
		},
		close: func() { _ = cli.CloseWithError(0, ""); _ = srv.CloseWithError(0, "") },
	}
}

func perfTLSPair(t *testing.T) perfPair {
	var ln net.Listener
	asSide(serverLabels, func() {
		var err error
		ln, err = tls.Listen("tcp", "127.0.0.1:0", perfTLS(t))
		require.NoError(t, err)
	})
	t.Cleanup(func() { ln.Close() })
	cfg := perfTLS(t)
	return perfPair{
		open: func(ctx context.Context) (perfStream, error) {
			var c *tls.Conn
			var err error
			asSide(clientLabels, func() {
				d := tls.Dialer{Config: cfg}
				var nc net.Conn
				nc, err = d.DialContext(ctx, "tcp", ln.Addr().String())
				if err == nil {
					c = nc.(*tls.Conn)
				}
			})
			return c, err
		},
		accept: func(ctx context.Context) (perfStream, error) {
			c, err := ln.Accept()
			if err != nil {
				return nil, err
			}
			return c.(*tls.Conn), nil
		},
		close: func() {},
	}
}

func perfPairOf(t *testing.T, mode string) perfPair {
	switch mode {
	case "quicgo":
		return perfQuicGoPair(t)
	case "tls":
		return perfTLSPair(t)
	}
	return perfMuxPair(t, mode)
}

// serve answers every stream: 'D' reads to EOF and acks with one byte, 'E' echoes.
func (p perfPair) serve(ctx context.Context) {
	asSide(serverLabels, func() {
		go func() {
			for {
				s, err := p.accept(ctx)
				if err != nil {
					return
				}
				go func() {
					defer s.Close()
					var op [1]byte
					if _, err := io.ReadFull(s, op[:]); err != nil {
						return
					}
					switch op[0] {
					case 'D':
						_, _ = io.Copy(io.Discard, s)
						_, _ = s.Write([]byte{1})
					case 'E':
						_, _ = io.Copy(s, s)
					}
				}()
			}
		}()
	})
}

// bulk sends size bytes on a new stream and waits for the server to have read them all.
func (p perfPair) bulk(ctx context.Context, size int, pace time.Duration, chunk []byte) error {
	s, err := p.open(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	if _, err := s.Write([]byte{'D'}); err != nil {
		return err
	}
	next := time.Now()
	for sent := 0; sent < size; sent += len(chunk) {
		if _, err := s.Write(chunk); err != nil {
			return err
		}
		if pace > 0 {
			next = next.Add(pace)
			if w := time.Until(next); w > 0 {
				time.Sleep(w)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err := s.CloseWrite(); err != nil {
		return err
	}
	var ack [1]byte
	_, err = io.ReadFull(s, ack[:])
	return err
}

// rtts does n round trips of a small message on one echo stream.
func (p perfPair) rtts(ctx context.Context, n int) ([]time.Duration, error) {
	s, err := p.open(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	msg := make([]byte, 64)
	msg[0] = 'E'
	if _, err := s.Write(msg[:1]); err != nil {
		return nil, err
	}
	buf := make([]byte, len(msg))
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		if _, err := s.Write(msg); err != nil {
			return out, err
		}
		if _, err := io.ReadFull(s, buf); err != nil {
			return out, err
		}
		out = append(out, time.Since(t0))
	}
	return out, nil
}

func perfProfile(t *testing.T, name string, f func()) {
	dir := os.Getenv("ECHOFOG_PERF_PROFILE")
	if dir == "" {
		f()
		return
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))
	file, err := os.Create(filepath.Join(dir, name+".pprof"))
	require.NoError(t, err)
	defer file.Close()
	require.NoError(t, pprof.StartCPUProfile(file))
	defer pprof.StopCPUProfile()
	f()
}

func perfSelectedModes() []string {
	if v := os.Getenv("ECHOFOG_PERF_MODES"); v != "" {
		return strings.Split(v, ",")
	}
	return perfModes
}

// TestPerfTransports: throughput and CPU of one bulk stream, CPU at the lab's 47 Mbit/s, and the
// round trip of a small message idle and beside a bulk stream.
func TestPerfTransports(t *testing.T) {
	perf.Skip(t)
	size := perf.EnvInt("ECHOFOG_PERF_MB", 64) << 20
	reps := perf.Repeat(5)
	chunk := make([]byte, 64<<10)
	_, _ = rand.Read(chunk)
	for _, mode := range perfSelectedModes() {
		t.Run(mode, func(t *testing.T) {
			p := perfPairOf(t, mode)
			defer p.close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			p.serve(ctx)
			require.NoError(t, p.bulk(ctx, 4<<20, 0, chunk), "warm-up")

			var mbps, cpuPerMB, corePct []float64
			perfProfile(t, mode+"-bulk", func() {
				pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), clientLabels))
				for i := 0; i < reps; i++ {
					var err error
					u := perf.Measure(func() { err = p.bulk(ctx, size, 0, chunk) })
					require.NoError(t, err)
					mb := float64(size) / (1 << 20)
					mbps = append(mbps, mb/u.Wall.Seconds())
					cpuPerMB = append(cpuPerMB, float64(u.CPU.Microseconds())/1000/mb)
					corePct = append(corePct, u.CorePct())
				}
			})

			// the lab's rate: 47 Mbit/s for 5 s, 16 KB writes
			paced := make([]byte, 16<<10)
			interval := time.Duration(float64(len(paced)*8) / 47e6 * float64(time.Second))
			pacedBytes := int(47e6 / 8 * 5)
			var pacedPct, pacedCPUPerMB []float64
			perfProfile(t, mode+"-paced", func() {
				for i := 0; i < reps; i++ {
					var err error
					u := perf.Measure(func() { err = p.bulk(ctx, pacedBytes, interval, paced) })
					require.NoError(t, err)
					pacedPct = append(pacedPct, u.CorePct())
					pacedCPUPerMB = append(pacedCPUPerMB, float64(u.CPU.Microseconds())/1000/(float64(pacedBytes)/(1<<20)))
				}
			})

			idle, err := p.rtts(ctx, 2000)
			require.NoError(t, err)
			var loaded []time.Duration
			var stop atomic.Bool
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stop.Load() {
					if err := p.bulk(ctx, 16<<20, 0, chunk); err != nil {
						return
					}
				}
			}()
			time.Sleep(200 * time.Millisecond)
			loaded, err = p.rtts(ctx, 500)
			stop.Store(true)
			wg.Wait()
			require.NoError(t, err)

			si, sl := perf.Summarize(perf.Durations(idle)), perf.Summarize(perf.Durations(loaded))
			perf.Record(t, "transport", map[string]any{
				"mode": mode, "bulk_MBps": perf.Summarize(mbps), "bulk_cpu_ms_per_MB": perf.Summarize(cpuPerMB),
				"bulk_core_pct": perf.Summarize(corePct), "paced47_core_pct": perf.Summarize(pacedPct),
				"paced47_cpu_ms_per_MB": perf.Summarize(pacedCPUPerMB),
				"rtt_idle_ms": si, "rtt_loaded_ms": sl,
			})
			t.Logf("%-8s bulk %s MB/s, %s CPU-ms/MB (%s %% core) | 47 Mbit/s: %s %% core, %s CPU-ms/MB | rtt idle p50 %.3f p99 %.3f ms, loaded p50 %.2f p99 %.2f ms",
				mode, perf.Summarize(mbps), perf.Summarize(cpuPerMB), perf.Summarize(corePct), perf.Summarize(pacedPct),
				perf.Summarize(pacedCPUPerMB), si.Med, si.P99, sl.Med, sl.P99)
		})
	}
}

// TestPerfDial: time and CPU to a connected, authenticated peer, on loopback (the CPU part) and
// through a path with 50 ms each way (100 ms RTT): the second divided by the RTT is how many round
// trips a dial costs.
func TestPerfDial(t *testing.T) {
	perf.Skip(t)
	reps := perf.Repeat(5)
	for _, mode := range perfSelectedModes() {
		if mode == "quicgo" || mode == "tls" {
			continue
		}
		t.Run(mode, func(t *testing.T) {
			srv := perfStartServer(t, mode)
			drain := make(chan struct{})
			defer close(drain)
			go func() {
				for {
					select {
					case ev := <-srv.events:
						go func() { time.Sleep(2 * time.Second); _ = ev.Conn.CloseWithError(0, "") }()
					case <-drain:
						return
					}
				}
			}()
			dial := func(addr string) (time.Duration, time.Duration) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				var done func()
				var err error
				u := perf.Measure(func() { _, done, err = srv.dial(ctx, addr) })
				require.NoError(t, err)
				done()
				return u.Wall, u.CPU
			}
			dial(srv.addr) // warm-up
			var lo, loCPU []time.Duration
			for i := 0; i < reps*4; i++ {
				w, c := dial(srv.addr)
				lo, loCPU = append(lo, w), append(loCPU, c)
			}
			var far []time.Duration
			const oneWay = 50 * time.Millisecond
			for i := 0; i < reps; i++ {
				var addr string
				if srv.udp {
					path, err := perf.NewUDPPath(srv.addr, oneWay)
					require.NoError(t, err)
					defer path.Close()
					addr = path.Addr()
				} else {
					path, err := perf.NewTCPPath(srv.addr, oneWay)
					require.NoError(t, err)
					defer path.Close()
					addr = path.Addr()
				}
				w, _ := dial(addr)
				far = append(far, w)
			}
			sLo, sCPU, sFar := perf.Summarize(perf.Durations(lo)), perf.Summarize(perf.Durations(loCPU)), perf.Summarize(perf.Durations(far))
			perf.Record(t, "dial", map[string]any{"mode": mode, "loopback_ms": sLo, "cpu_ms": sCPU, "rtt100_ms": sFar,
				"round_trips": sFar.Med / 100})
			t.Logf("%-8s dial loopback %s ms (CPU %s ms, both ends) | at 100 ms RTT %s ms = %.1f RTT", mode, sLo, sCPU, sFar, sFar.Med/100)
		})
	}
}
