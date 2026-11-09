package node_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
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

const (
	coverEchoID      uint64 = service.ExperimentalMin
	coverEchoVersion uint64 = 1
	coverEchoFrame   uint64 = 0x40
)

var coverEchoSpec = service.Spec{
	ID: coverEchoID, Name: "cover-echo",
	Versions: map[uint64]service.Version{
		coverEchoVersion: {Types: []service.FrameType{{Type: coverEchoFrame, Name: "ECHO", Max: 4096}}},
	},
}

func registerCoverEcho(rt *node.Runtime) {
	rt.Register(coverEchoSpec, func(_ context.Context, _ node.Peer, st *mux.Stream) {
		defer st.Close()
		for {
			f, err := st.ReadFrame()
			if err != nil {
				return
			}
			if err := st.WriteFrame(coverEchoFrame, 0, f.Payload); err != nil {
				return
			}
		}
	})
}

// freeTCPPort and freeUDPPort return a port that was just free on loopback.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func coverSite(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>welcome to the cover</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// coverServer starts one own-cover node with a reality2-quic and/or reality2-tcp address and
// returns its runtime, the peer card of the server and the listener. The client is a separate
// runtime of the same test.
func coverServer(t *testing.T, tcpPort, udpPort int, site, domain string) (*node.Runtime, *objects.Card, carrier.Listener) {
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

	var cardListeners []cards.Listener
	var addrs []string
	if tcpPort != 0 {
		cardListeners = append(cardListeners, cards.Listener{
			Suite: objects.SuiteReality2TCP, Host: "127.0.0.1", Port: uint32(tcpPort), SNI: domain, Mask: maskPub,
		})
		addrs = append(addrs, net.JoinHostPort("127.0.0.1", strconv.Itoa(tcpPort)))
	}
	if udpPort != 0 {
		cardListeners = append(cardListeners, cards.Listener{
			Suite: objects.SuiteReality2QUIC, Host: "127.0.0.1", Port: uint32(udpPort), SNI: domain, Mask: maskPub,
		})
		addrs = append(addrs, net.JoinHostPort("127.0.0.1", strconv.Itoa(udpPort)))
	}
	builder, err := cards.New(cards.Config{
		Account: tw.Account.Public(), Device: tw.Device, HsKey: tw.Device.Handshake().Public(),
		L1Suites: []uint32{uint32(1), uint32(2)}, Role: objects.RoleServer,
		Listeners: cardListeners, Devices: devices, Visibility: cards.Visible,
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(store.Options{Dir: t.TempDir(), Key: tw.Account.StorageKey()})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := node.SelfSignedCertificate(domain)
	if err != nil {
		t.Fatal(err)
	}
	src := node.Reality2Listener{
		Domain: domain, SiteDir: site, Cert: cert,
		CertKV: st.Store("v2/reality2/self/cert"), ReplayKV: st.Store("v2/reality2/replay"),
		Masks: []reality2.MaskKey{{Private: mask}}, Addrs: addrs,
	}
	if tcpPort != 0 {
		src.TCPAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(tcpPort))
	}
	if udpPort != 0 {
		src.UDPAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(udpPort))
	}
	l, err := src.Listen(t.Context())
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	rt, err := node.New(node.Config{
		Account: tw.Account, Device: tw.Device, Cards: builder, Store: st,
		Profile: node.Profile{Platform: node.PlatformServer},
		// The listener was built above so the card could carry its real ports; the runtime takes
		// it over and starts the accept loop.
		Listeners: []carrier.Listener{l},
	})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	registerCoverEcho(rt)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Stop(ctx)
		_ = st.Close()
	})
	if err := rt.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	env, ok := rt.Cards().Own(objects.ScopePeer)
	if !ok {
		t.Fatal("the server has no peer card")
	}
	card, err := objects.VerifyCard(env, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rt, card, l
}

// TestReality2StrangerGetsCover: a TLS client without a token gets the operator's real site, not
// a refusal (T2.6b-2, T2.6e item 1). The cover is the own-cover mode with a static certificate.
func TestReality2StrangerGetsCover(t *testing.T) {
	domain := "cover.test"
	port := freeTCPPort(t)
	_, _, l := coverServer(t, port, 0, coverSite(t), domain)
	_ = l

	raw, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial cover: %v", err)
	}
	defer raw.Close()
	tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, ServerName: domain, NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("cover handshake: %v", err)
	}
	if _, err := fmt.Fprintf(tc, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", domain); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(io.LimitReader(tc, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "welcome to the cover") {
		t.Fatalf("a stranger did not get the cover page:\n%s", body)
	}
}

// TestReality2CarrierSessions: an efv2-shaped client reaches the server node over reality2-tcp and
// reality2-quic, opens a service stream and gets its bytes back (T2.6e items 1 and 6).
func TestReality2CarrierSessions(t *testing.T) {
	domain := "cover.test"
	tcpPort, udpPort := freeTCPPort(t), freeUDPPort(t)
	_, card, _ := coverServer(t, tcpPort, udpPort, coverSite(t), domain)

	client := newTestNodeWith(t, cards.Visible, false, nil, nil, func(c *node.Config) {
		// The lab client and the server are in one country: the D21 plan keeps a direct
		// camouflage carrier (T6.4).
		lab := func(netip.Addr) (string, bool) { return "lab", true }
		c.Country = lab
		c.LocalInfo = carrier.LocalInfo{CountryOf: lab}
	})
	client.start(t)
	registerCoverEcho(client.rt)
	dev := node.DeviceID(card.DeviceID)

	for _, suite := range []uint32{objects.SuiteReality2TCP, objects.SuiteReality2QUIC} {
		t.Run(fmt.Sprintf("suite-%d", suite), func(t *testing.T) {
			var addr *pb.Addr
			for _, a := range card.Addrs() {
				if a.GetSuite() == suite {
					addr = a
					break
				}
			}
			if addr == nil {
				t.Fatalf("the card has no address of suite %d", suite)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			attempts := []carrier.Attempt{{Kind: carrier.AttemptCarrier, Suite: suite, Addr: addr}}
			_, st, err := client.rt.Open(ctx, dev, coverEchoID, node.DialOpt{Card: card, Attempts: attempts})
			if err != nil {
				t.Fatalf("open over suite %d: %v", suite, err)
			}
			defer st.Close()
			if err := st.WriteFrame(coverEchoFrame, 0, []byte("hello over reality2")); err != nil {
				t.Fatalf("write: %v", err)
			}
			f, err := st.ReadFrame()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if f.Type != coverEchoFrame || string(f.Payload) != "hello over reality2" {
				t.Fatalf("echo = type %d %q", f.Type, f.Payload)
			}
		})
	}
}

// TestWS2ListenerServesCover: a plain request to the ws2 listener's non-secret path gets the
// operator's site, not the WebSocket back end (T2.9, T2.6e item 1).
func TestWS2ListenerServesCover(t *testing.T) {
	port := freeTCPPort(t)
	src := node.WS2Listener{
		Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Path: "/secret-path",
		Domain: "cover.test", SiteDir: coverSite(t),
		Masks: []reality2.MaskKey{{Private: mustX25519(t)}},
		Addrs: []string{net.JoinHostPort("cover.test", "443")},
	}
	l, err := src.Listen(t.Context())
	if err != nil {
		t.Fatalf("ws2 listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	resp, err := http.Get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if !strings.Contains(string(body), "welcome to the cover") {
		t.Fatalf("ws2 did not serve the cover site:\n%s", body)
	}
}

func mustX25519(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// sanity: the test exercises the full accept path; nothing else is needed here.
