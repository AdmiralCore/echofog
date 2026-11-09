package servernode_test

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/carrier/ws2"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/servernode"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// ws2Client is a client runtime that knows the ws2 dialer and trusts the test CDN's certificate.
func ws2Client(t *testing.T, roots *x509.CertPool) *node.Runtime {
	t.Helper()
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatal(err)
	}
	devices, err := tw.DeviceList(1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	builder, err := cards.New(cards.Config{
		Account: tw.Account.Public(), Device: tw.Device, HsKey: tw.Device.Handshake().Public(),
		Role: objects.RoleUser, Devices: devices, Visibility: cards.Hidden,
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(store.Options{Dir: t.TempDir(), Key: tw.Account.StorageKey()})
	if err != nil {
		t.Fatal(err)
	}
	lab := func(netip.Addr) (string, bool) { return "lab", true }
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
	rt, err := node.New(node.Config{
		Account: tw.Account, Device: tw.Device, Cards: builder, Store: st,
		Profile: node.Profile{Platform: node.PlatformDesktop}, Registry: reg,
		Country: lab, LocalInfo: carrier.LocalInfo{CountryOf: lab, Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
	})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Stop(c)
		_ = st.Close()
		tw.Destroy()
	})
	return rt
}

// TestServerWS2Session: the server node serves ws2 behind a local TLS reverse proxy (the CDN).
// A client reads the ws2 address from the server's public card, opens a session and gets the same
// peer: the product path from server.json to a live session, not just the cover page (T2.6e
// "Готово"; the node-level test in internal/v2/node also covers the message and the exit stream).
func TestServerWS2Session(t *testing.T) {
	site := coverSiteDir(t)
	backend := freeTCPPort(t)
	target, _ := url.Parse("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(backend)))
	cdn := httptest.NewUnstartedServer(httputil.NewSingleHostReverseProxy(target))
	cdn.StartTLS()
	t.Cleanup(cdn.Close)
	roots := x509.NewCertPool()
	roots.AddCert(cdn.Certificate())
	cdnAddr := cdn.Listener.Addr().String()
	_, cdnPort, _ := net.SplitHostPort(cdnAddr)

	cfg := servernode.DefaultConfig()
	cfg.Roles = []string{"relay"}
	cfg.Listen = []servernode.ListenSpec{{Suite: "ws2", Bind: net.JoinHostPort("127.0.0.1", strconv.Itoa(backend)), Advertise: "127.0.0.1"}}
	cfg.Cover = "127.0.0.1:" + cdnPort
	cfg.SiteDir = site
	cfg.CoverCert = writeCertKey(t, "127.0.0.1", t.TempDir())
	dir := filepath.Join(t.TempDir(), "node")
	if _, err := servernode.Init(dir, cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv, _ := startServer(t, dir)
	st := srv.Status()
	if !st.PublicCard || st.PublicLink == "" {
		t.Fatal("the ws2 server has no public card")
	}
	link, err := objects.ParseLink(st.PublicLink)
	if err != nil {
		t.Fatal(err)
	}
	card, err := objects.VerifyCard(link.Card, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range card.Addrs() {
		found = found || a.GetSuite() == objects.SuiteWS2
	}
	if !found {
		t.Fatalf("the public card has no ws2 address: %s", fmt.Sprint(card.Addrs()))
	}

	client := ws2Client(t, roots)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	peer, sess, err := client.Session(ctx, node.DeviceID(card.DeviceID), node.DialOpt{Card: card})
	if err != nil {
		t.Fatalf("a ws2 session from the public card: %v", err)
	}
	_ = peer
	if sess == nil {
		t.Fatal("no session")
	}
	_ = sess.Close()
}
