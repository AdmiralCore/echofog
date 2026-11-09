package servernode_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/servernode"
)

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// freePort returns a port that is free for both TCP and UDP on the loopback address.
func freePort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 20; i++ {
		p := freeTCPPort(t)
		pc, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			_ = pc.Close()
			return p
		}
	}
	t.Fatal("no port free for both TCP and UDP")
	return 0
}

// TestPublicCardDoesNotCarryTheNoise3MaskKey: wire.md §2.1 says noise3 keys are never published.
// The public card of a server that serves reality2 and noise3 carries the masking key of the
// camouflaged carriers only, and that key is not the public part of the profile's noise3 mask.
// The reality2 listeners still accept a session opened with the published key.
func TestPublicCardDoesNotCarryTheNoise3MaskKey(t *testing.T) {
	site := t.TempDir()
	if err := os.WriteFile(filepath.Join(site, "index.html"), []byte("cover"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	cfg := servernode.DefaultConfig()
	cfg.Roles = []string{"relay"}
	cfg.Listen = []servernode.ListenSpec{
		{Suite: "reality2-quic", Bind: fmt.Sprintf("127.0.0.1:%d", port), Advertise: "127.0.0.1"},
		{Suite: "reality2-tcp", Bind: fmt.Sprintf("127.0.0.1:%d", port), Advertise: "127.0.0.1"},
		{Suite: "noise3", Bind: fmt.Sprintf("127.0.0.1:%d", port+1), Advertise: "127.0.0.1"},
	}
	cfg.Cover = fmt.Sprintf("example.org:%d", port)
	cfg.SiteDir = site
	cfg.CoverCert = writeCertKey(t, "example.org", t.TempDir())
	dir := filepath.Join(t.TempDir(), "node")
	if _, err := servernode.Init(dir, cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv, _ := startServer(t, dir)

	st := srv.Status()
	if !st.PublicCard || st.PublicLink == "" {
		t.Fatal("a camouflaged server has no public card (D37)")
	}
	link, err := objects.ParseLink(st.PublicLink)
	if err != nil {
		t.Fatalf("parse the public link: %v", err)
	}
	card, err := objects.VerifyCard(link.Card, time.Now())
	if err != nil {
		t.Fatalf("verify the public card: %v", err)
	}
	masks := card.Body.GetMaskKeys()
	if len(masks) != 1 {
		t.Fatalf("public card carries %d masking keys, want 1 for the camouflaged pair", len(masks))
	}

	// The noise3 key of the profile: it accepts the disguised carrier and must never be published.
	raw, err := os.ReadFile(filepath.Join(dir, "server-identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idf struct {
		MaskPrivate string `json:"mask_private"`
	}
	if err := json.Unmarshal(raw, &idf); err != nil {
		t.Fatal(err)
	}
	priv, err := hex.DecodeString(idf.MaskPrivate)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	noisePub := k.PublicKey().Bytes()
	published, err := keys.ParseKind(masks[0].GetKey(), keys.X25519)
	if err != nil {
		t.Fatalf("the public card mask key: %v", err)
	}
	if bytes.Equal(published.Key(), noisePub) {
		t.Fatal("the public card publishes the masking key that accepts noise3 (wire.md §2.1: noise3 keys are never published)")
	}

	// The published key is the one the reality2 listener accepts: a client opens a session with it.
	var addr *pb.Addr
	for _, a := range card.Addrs() {
		if a.GetSuite() == objects.SuiteReality2TCP {
			addr = a
			break
		}
	}
	if addr == nil {
		t.Fatal("the public card has no reality2-tcp address")
	}
	client := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, sess, err := client.Session(ctx, node.DeviceID(card.DeviceID), node.DialOpt{
		Card:     card,
		Attempts: []carrier.Attempt{{Kind: carrier.AttemptCarrier, Suite: objects.SuiteReality2TCP, Addr: addr}},
	})
	if err != nil {
		t.Fatalf("a session with the published masking key: %v", err)
	}
	_ = sess.Close()
}
