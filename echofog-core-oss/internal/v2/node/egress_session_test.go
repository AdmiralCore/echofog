package node_test

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/carrier/ws2"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
)

// recordingDialer is a suite dialer that keeps the requests the node hands to it and then dials
// normally. The positive egress test uses it to see what the node asks a carrier to do: a proxy
// hook and no MSS opener.
type recordingDialer struct {
	inner carrier.Dialer
	mu    sync.Mutex
	reqs  []carrier.DialRequest
}

func (d *recordingDialer) Dial(ctx context.Context, req carrier.DialRequest) (mux.Carrier, error) {
	d.mu.Lock()
	d.reqs = append(d.reqs, req)
	d.mu.Unlock()
	return d.inner.Dial(ctx, req)
}

func (d *recordingDialer) requests() []carrier.DialRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]carrier.DialRequest(nil), d.reqs...)
}

// egressPeerListener is a real listener of node b. The card names it by a host name, so only the
// proxy can resolve it: a direct dial of the test node has nowhere to go. The listener is bound to
// the loopback and the proxy redirects the CONNECT there.
func egressPeerListener(t *testing.T, suite uint32) (ln carrier.Listener, cl cards.Listener, redirect string, roots *x509.CertPool) {
	t.Helper()
	sk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mask := keys.New(keys.X25519, sk.PublicKey().Bytes())
	cert, roots := egressTestCertificate(t, "b.example")
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(raw.Addr().(*net.TCPAddr).Port)
	hostPort := net.JoinHostPort("b.example", strconv.Itoa(int(port)))
	innerTLS := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}

	switch suite {
	case objects.SuiteReality2TCP:
		l, err := reality2.ListenTCP(raw, reality2.TCPListenerConfig{
			ServerTLS: innerTLS,
			Masks:     []reality2.MaskKey{{Private: sk}},
			Addrs:     []string{hostPort},
			Stranger:  func(c net.Conn, _ []byte) { _ = c.Close() },
		})
		if err != nil {
			t.Fatal(err)
		}
		return l, cards.Listener{Suite: suite, Host: "b.example", Port: port, SNI: "cover.example", Mask: mask},
			raw.Addr().String(), roots
	case objects.SuiteWS2:
		l, err := ws2.Listen(tls.NewListener(raw, innerTLS), ws2.Config{
			Path:      "/b",
			ServerTLS: innerTLS,
			Masks:     []reality2.MaskKey{{Private: sk}},
			Addrs:     []string{hostPort},
		})
		if err != nil {
			t.Fatal(err)
		}
		return l, cards.Listener{Suite: suite, URL: "wss://" + hostPort + "/b", SNI: "cover.example", Mask: mask},
			raw.Addr().String(), roots
	default:
		t.Fatalf("no test listener for suite %d", suite)
		return nil, cards.Listener{}, "", nil
	}
}

// egressTestCertificate is a self-signed server certificate for the names of the test; the outer
// TLS of ws2 checks it, the inner reality2 TLS does not.
func egressTestCertificate(t *testing.T, names ...string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: names[0]},
		DNSNames:              names,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

// egressCarrierRegistry is the registry of node a: the real dialers, wrapped to record the request,
// and the test roots for the outer TLS of ws2 (the prefix of the card is not resolvable and the
// proxy redirects it to the local test site).
func egressCarrierRegistry(roots *x509.CertPool) (*carrier.Registry, *recordingDialer, *recordingDialer) {
	reg := carrier.NewRegistry()
	tcp := &recordingDialer{inner: reality2.NewTCPDialer(reality2.TCPDialerConfig{})}
	web := &recordingDialer{inner: ws2.NewDialer(ws2.DialerConfig{RootCAs: roots})}
	reg.RegisterDialer(objects.SuiteReality2TCP, tcp)
	reg.RegisterDialer(objects.SuiteWS2, web)
	return reg, tcp, web
}

// TestSessionOpensThroughTheProxy is the positive side of the leak test (T11.6a M1): the node
// behind a SOCKS5 proxy opens a session over reality2-tcp and over ws2, and the proxy is asked for
// the peer's TCP address as a name. Nothing in the node resolves the name itself. The dial request
// the carrier receives carries the proxy hook, no MSS opener (a proxy opens its own socket) and the
// narrow-path/network state; the probe path keeps the same proxy hook.
func TestSessionOpensThroughTheProxy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		suite uint32
		probe bool
	}{
		{"reality2-tcp", objects.SuiteReality2TCP, false},
		{"ws2", objects.SuiteWS2, false},
		{"reality2-tcp probe", objects.SuiteReality2TCP, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, cl, redirect, roots := egressPeerListener(t, tc.suite)
			reg, tcpRec, webRec := egressCarrierRegistry(roots)

			px := newFakeSocks(t)
			px.redirect = redirect

			b := newTestNodeFull(t, cards.Visible, false, []carrier.Listener{ln}, nil,
				func(c *cards.Config) { c.Listeners = []cards.Listener{cl} }, nil)
			a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) {
				c.Egress = egressTo(px, false)
				c.Registry = reg
			})
			b.start(t)
			a.start(t)

			if tc.probe {
				// A foreign destination from a Russian mobile network: the row forbids a direct
				// session but allows a probe with a byte budget.
				a.rt.SetCountry("RU")
				a.rt.SetNetClass(rp.NetMobile)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			peer, sess, err := a.rt.Session(ctx, b.id.DeviceID, node.DialOpt{
				Card:  b.peerCard(t),
				Probe: tc.probe,
			})
			if err != nil {
				t.Fatalf("session through the proxy: %v", err)
			}
			defer sess.Close()
			if peer.Device != b.id.DeviceID {
				t.Fatalf("peer device %x, want %x", peer.Device, b.id.DeviceID)
			}

			want := net.JoinHostPort("b.example", strconv.Itoa(int(cl.Port)))
			if tc.suite == objects.SuiteWS2 {
				hostPort, _, err := ws2.ParseURL(cl.URL)
				if err != nil {
					t.Fatal(err)
				}
				want = hostPort
			}
			asked := px.requests()
			if len(asked) != 1 {
				t.Fatalf("the proxy was asked %v, want exactly one CONNECT", asked)
			}
			if asked[0] != want {
				t.Fatalf("the proxy was asked for %q, want the card name %q", asked[0], want)
			}

			// The node hands the suite the proxy hook, no MSS opener and the network state.
			rec := tcpRec
			if tc.suite == objects.SuiteWS2 {
				rec = webRec
			}
			reqs := rec.requests()
			if len(reqs) == 0 {
				t.Fatal("the suite dialer saw no request")
			}
			for _, req := range reqs {
				if req.DialContext == nil {
					t.Fatal("the carrier got no proxy hook")
				}
				if req.DialMSS != nil {
					t.Fatal("the carrier got an MSS opener behind a proxy")
				}
				if req.Narrow == nil || req.NetworkID == nil {
					t.Fatal("the carrier got no narrow-path or network state")
				}
			}
			if tc.probe {
				rep, ok := a.rt.RouteReport(b.id.DeviceID)
				if !ok || rep.RuleID != "ru-mobile-foreign" {
					t.Fatalf("route report %+v %v, want the probe row", rep, ok)
				}
			}
		})
	}
}
