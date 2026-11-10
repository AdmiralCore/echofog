package mobile

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

type fakeCallbacks struct {
	protected atomic.Int32
}

func (f *fakeCallbacks) Protect(int) error  { f.protected.Add(1); return nil }
func (f *fakeCallbacks) LocalAddrs() string { return "127.0.0.1\n" }

// reality2Source adapts a bound reality2 listener to node.ListenerSource.
type reality2Source struct{ ln carrier.Listener }

func (s reality2Source) Listen(context.Context) (carrier.Listener, error) { return s.ln, nil }

func TestVersion(t *testing.T) {
	if Version() == "" {
		t.Fatal("empty version")
	}
}

func TestStartStopLifecycle(t *testing.T) {
	cfg, _ := json.Marshal(config{DataDir: t.TempDir()})
	cb := &fakeCallbacks{}
	if err := Start(string(cfg), cb); err != nil {
		t.Fatal(err)
	}
	if err := Start(string(cfg), cb); err == nil {
		t.Fatal("second Start succeeded")
	}
	if _, err := Dial(`{"envelope":"!"}`); err == nil {
		t.Fatal("Dial accepted a broken card")
	}
	if err := Stop(); err != nil {
		t.Fatal(err)
	}
	if err := Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := Start(`{"data_dir":""}`, cb); err == nil {
		t.Fatal("Start without data_dir succeeded")
	}
}

// TestReality2DialWithoutGODEBUG is the T12.6 canary: the runtime started through the binding,
// with the android profile and no GODEBUG, opens a reality2-quic session to a host node and
// pings it. The test process sets no cryptocustomrand default.
func TestReality2DialWithoutGODEBUG(t *testing.T) {
	if strings.Contains(os.Getenv("GODEBUG"), "cryptocustomrand") {
		t.Skip("GODEBUG=cryptocustomrand is set; the canary must run without it")
	}
	cover := newCover(t)
	cert := testCert(t)
	var hostCard string
	var host *instance

	hostCfg := config{DataDir: t.TempDir(), Platform: "server"}
	host, err := open(hostCfg, nil, func(dev *identity.Device) ([]cards.Listener, []node.ListenerSource, error) {
		var maskPriv *ecdh.PrivateKey
		if err := dev.Masking().WithPrivate(func(b []byte) (err error) {
			maskPriv, err = ecdh.X25519().NewPrivateKey(b)
			return
		}); err != nil {
			return nil, nil, err
		}
		udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return nil, nil, err
		}
		port := udp.LocalAddr().(*net.UDPAddr).Port
		ln, err := reality2.Listen(udp, reality2.ListenerConfig{
			Cover:     reality2.CoverProfile{Group: tls.X25519, Cert: &cert},
			Masks:     []reality2.MaskKey{{Private: maskPriv}},
			Addrs:     []string{reality2.AddressString("127.0.0.1", uint32(port))},
			CoverAddr: cover,
		})
		if err != nil {
			return nil, nil, err
		}
		return []cards.Listener{{
			Suite: objects.SuiteReality2QUIC, Host: "127.0.0.1", Port: uint32(port),
			SNI: "cover.example", Mask: keys.New(keys.X25519, maskPriv.PublicKey().Bytes()),
		}}, []node.ListenerSource{reality2Source{ln}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := host.rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer host.stop()
	ic, err := host.cards.Current(ctx, objects.ScopePeer)
	if err != nil {
		t.Fatal(err)
	}
	hostCard = base64.StdEncoding.EncodeToString(ic.Envelope)

	cb := &fakeCallbacks{}
	cfg, _ := json.Marshal(config{DataDir: t.TempDir(), Platform: "android", LowPower: true})
	if err := Start(string(cfg), cb); err != nil {
		t.Fatal(err)
	}
	defer Stop()
	// The canary has no GeoIP database (countryOf answers nothing), and the policy's "unknown" row
	// keeps QUIC off (T6.4c), while this canary must prove reality2-quic without GODEBUG (T12.6).
	// A known country puts the client on the foreign row, which allows the carrier.
	cur.rt.SetCountry("DE")
	out, err := Dial(`{"envelope":"` + hostCard + `"}`)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	var res struct {
		PeerDeviceID string  `json:"peer_device_id"`
		RTTMs        float64 `json:"rtt_ms"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.PeerDeviceID == "" {
		t.Fatalf("result %q: %v", out, err)
	}
	// The host card lists a single address, reality2-quic, so a session can only have come in
	// through that carrier. (SessionStatus.Suite is the L1 handshake suite, not the carrier.)
	if n := len(host.rt.Status().Sessions); n != 1 {
		t.Fatalf("host has %d sessions, want 1", n)
	}
}

func newCover(t *testing.T) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDP(append([]byte("cover:"), buf[:n]...), src)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr)
}

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}
