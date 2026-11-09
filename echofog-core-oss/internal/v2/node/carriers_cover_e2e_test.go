package node_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"io"
	"net"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/carrier/ws2"
	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/store"
)

const coverDomain = "cover.test"

// freeBothPort returns a port free for TCP and UDP on loopback. The scan is deterministic so a
// run does not pick a port an earlier test still holds.
func freeBothPort(t *testing.T) int {
	t.Helper()
	for p := 20000; p < 40000; p++ {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			_ = ln.Close()
			continue
		}
		_ = pc.Close()
		_ = ln.Close()
		return p
	}
	t.Fatal("no port free for both TCP and UDP")
	return 0
}

// carrierServer is one server node that serves the operator's site and one camouflaged carrier.
type carrierServer struct {
	rt    *node.Runtime
	card  *objects.Card
	addr  *pb.Addr
	roots *x509.CertPool
}

// startCarrierServer brings up a server node whose only carrier is the given suite: reality2-quic,
// reality2-tcp or ws2. It uses the same node.Reality2Listener and node.WS2Listener constructors
// the server node uses; ws2 sits behind an httptest TLS reverse proxy that plays the CDN.
func startCarrierServer(t *testing.T, suite string) *carrierServer {
	t.Helper()
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Destroy)
	devices, err := tw.DeviceList(1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	mask, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	maskPub := keys.New(keys.X25519, mask.PublicKey().Bytes())
	maskKey := reality2.MaskKey{Private: mask}

	site := coverSite(t)
	cs := &carrierServer{}
	st, err := store.Open(store.Options{Dir: t.TempDir(), Key: tw.Account.StorageKey()})
	if err != nil {
		t.Fatal(err)
	}
	var cardListeners []cards.Listener
	var listeners []carrier.Listener

	switch suite {
	case "reality2-quic", "reality2-tcp":
		port := freeBothPort(t)
		bind := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		var number uint32 = objects.SuiteReality2TCP
		if suite == "reality2-quic" {
			number = objects.SuiteReality2QUIC
		}
		cardListeners = append(cardListeners, cards.Listener{Suite: number, Host: "127.0.0.1", Port: uint32(port), SNI: coverDomain, Mask: maskPub})
		cert, err := node.SelfSignedCertificate(coverDomain)
		if err != nil {
			t.Fatal(err)
		}
		src := node.Reality2Listener{
			Domain: coverDomain, SiteDir: site, Cert: cert,
			CertKV: st.Store("v2/reality2/self/cert"), ReplayKV: st.Store("v2/reality2/replay"),
			Masks: []reality2.MaskKey{maskKey}, Addrs: []string{bind},
		}
		if suite == "reality2-quic" {
			src.UDPAddr = bind
		} else {
			src.TCPAddr = bind
		}
		l, err := src.Listen(t.Context())
		if err != nil {
			t.Fatalf("%s listen: %v", suite, err)
		}
		t.Cleanup(func() { _ = l.Close() })
		listeners = append(listeners, l)
		cs.addr = &pb.Addr{Suite: number, Host: "127.0.0.1", Port: uint32(port)}
	case "ws2":
		backend := freeTCPPort(t)
		target, _ := url.Parse("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(backend)))
		cdn := httptest.NewUnstartedServer(httputil.NewSingleHostReverseProxy(target))
		cdn.StartTLS()
		t.Cleanup(cdn.Close)
		cs.roots = x509.NewCertPool()
		cs.roots.AddCert(cdn.Certificate())
		cdnAddr := cdn.Listener.Addr().String()
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		path := "/" + hex.EncodeToString(raw)
		cardListeners = append(cardListeners, cards.Listener{Suite: objects.SuiteWS2, URL: "wss://" + cdnAddr + path, SNI: coverDomain, Mask: maskPub})
		l, err := node.WS2Listener{
			Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(backend)), Path: path, Domain: coverDomain, SiteDir: site,
			Masks: []reality2.MaskKey{maskKey}, Addrs: []string{cdnAddr},
		}.Listen(t.Context())
		if err != nil {
			t.Fatalf("ws2 listen: %v", err)
		}
		t.Cleanup(func() { _ = l.Close() })
		listeners = append(listeners, l)
		cs.addr = &pb.Addr{Suite: objects.SuiteWS2, Url: "wss://" + cdnAddr + path}
	default:
		t.Fatalf("unknown suite %q", suite)
	}

	builder, err := cards.New(cards.Config{
		Account: tw.Account.Public(), Device: tw.Device, HsKey: tw.Device.Handshake().Public(),
		L1Suites: []uint32{uint32(1), uint32(2)}, Role: objects.RoleServer, Exit: freeExit{},
		Listeners: cardListeners, Devices: devices, Visibility: cards.Visible,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := node.New(node.Config{
		Account: tw.Account, Device: tw.Device, Cards: builder, Store: st,
		Profile:   node.Profile{Platform: node.PlatformServer},
		Listeners: listeners,
	})
	if err != nil {
		t.Fatal(err)
	}
	registerCoverEcho(rt)
	// The real exit/2 server, opened for the local test target only (AllowPrivateTargets).
	spec, ok := service.Builtin(service.Exit)
	if !ok {
		t.Fatal("exit/2 is not built in")
	}
	exitSrv, err := exit.NewServer(exit.ServerConfig{AllowPrivateTargets: true})
	if err != nil {
		t.Fatal(err)
	}
	rt.Register(spec, func(ctx context.Context, p node.Peer, st *mux.Stream) {
		exitSrv.Handle(ctx, exit.Peer{Node: [32]byte(p.Node), Device: [32]byte(p.Device), SID: p.SID, Remote: p.Remote}, st)
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Stop(ctx)
		_ = st.Close()
	})
	if err := rt.Start(t.Context()); err != nil {
		t.Fatalf("%s start: %v", suite, err)
	}
	env, ok := rt.Cards().Own(objects.ScopePublic)
	if !ok {
		t.Fatal("the server has no peer card")
	}
	card, err := objects.VerifyCard(env, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Use the verified card's address, with its mask index, exactly as a client would.
	for _, a := range card.Addrs() {
		if a.GetSuite() == cs.addr.GetSuite() {
			cs.addr = a
			break
		}
	}
	cs.rt, cs.card = rt, card
	return cs
}

// carrierClient starts a client runtime whose registry knows the camouflaged suites; the ws2
// dialer trusts the test CDN's certificate. The route policy is left to choose the carrier, as in
// the product (T6.4).
func carrierClient(t *testing.T, roots *x509.CertPool) *testNode {
	t.Helper()
	return newTestNodeWith(t, cards.Visible, false, nil, nil, func(c *node.Config) {
		lab := func(netip.Addr) (string, bool) { return "lab", true }
		c.Country = lab
		c.LocalInfo = carrier.LocalInfo{CountryOf: lab, Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
		reg := carrier.NewRegistry()
		noise3.NewDialer(noise3.DialerConfig{}).Register(reg)
		if err := reality2.Register(reg, reality2.NewDialer(reality2.DialerConfig{}), nil); err != nil {
			t.Fatal(err)
		}
		if err := reality2.RegisterTCP(reg, reality2.NewTCPDialer(reality2.TCPDialerConfig{}), nil); err != nil {
			t.Fatal(err)
		}
		if err := ws2.Register(reg, ws2.NewDialer(ws2.DialerConfig{RootCAs: roots}), nil); err != nil {
			t.Fatal(err)
		}
		c.Registry = reg
	})
}

// echoTarget listens on loopback and echoes every byte: the destination of an exit stream.
func echoTarget(t *testing.T) string {
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
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// TestCarriersEndToEnd: for every server carrier — reality2-quic, reality2-tcp and ws2 — a client
// opens a session, exchanges a message on a service stream, and opens an exit stream that reaches
// a target and carries bytes back (T2.6e item 6 and "Готово": session, message, exit stream).
func TestCarriersEndToEnd(t *testing.T) {
	target := echoTarget(t)
	for _, suite := range []string{"reality2-quic", "reality2-tcp", "ws2"} {
		t.Run(suite, func(t *testing.T) {
			cs := startCarrierServer(t, suite)
			client := carrierClient(t, cs.roots)
			client.start(t)
			registerCoverEcho(client.rt)
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			dev := node.DeviceID(cs.card.DeviceID)

			// Session and message: a service stream carries a frame there and back.
			_, st, err := client.rt.Open(ctx, dev, coverEchoID, node.DialOpt{Card: cs.card})
			if err != nil {
				t.Fatalf("open the echo service over %s: %v", suite, err)
			}
			if err := st.WriteFrame(coverEchoFrame, 0, []byte("hello over "+suite)); err != nil {
				t.Fatalf("write over %s: %v", suite, err)
			}
			f, err := st.ReadFrame()
			if err != nil {
				t.Fatalf("read over %s: %v", suite, err)
			}
			_ = st.Close()
			if f.Type != coverEchoFrame || string(f.Payload) != "hello over "+suite {
				t.Fatalf("%s echo = type %d %q", suite, f.Type, f.Payload)
			}

			// Exit stream: the client reaches the exit service and its bytes go to the target.
			cl, err := client.rt.ExitClient(ctx, cs.card, node.DialOpt{Card: cs.card})
			if err != nil {
				t.Fatalf("exit client over %s: %v", suite, err)
			}
			defer cl.Close()
			conn, err := cl.DialExitStream(ctx, target)
			if err != nil {
				t.Fatalf("open an exit stream over %s: %v", suite, err)
			}
			defer conn.Close()
			msg := []byte("exit over " + suite)
			if _, err := conn.Write(msg); err != nil {
				t.Fatalf("write to the exit stream over %s: %v", suite, err)
			}
			got := make([]byte, len(msg))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("read from the exit stream over %s: %v", suite, err)
			}
			if string(got) != string(msg) {
				t.Fatalf("%s exit echo = %q", suite, got)
			}
		})
	}
}
