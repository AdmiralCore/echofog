package servernode_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/servernode"
	"github.com/echofog/echofog-core/internal/v2/store"
)

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// profile creates a server profile on loopback and returns its data directory and the port.
func profile(t *testing.T, invites bool) (string, servernode.Config) {
	t.Helper()
	cfg := servernode.DefaultConfig()
	cfg.Roles = []string{"relay", "mailbox"}
	cfg.Listen = []servernode.ListenSpec{{Suite: "noise3", Bind: fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t)), Advertise: "127.0.0.1"}}
	if invites {
		cfg.InviteListen = &servernode.ListenSpec{Suite: "noise3", Bind: fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t)), Advertise: "127.0.0.1"}
	}
	dir := filepath.Join(t.TempDir(), "node")
	phrase, err := servernode.Init(dir, cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if n := len(strings.Fields(phrase)); n != 12 {
		t.Fatalf("recovery phrase has %d words", n)
	}
	if _, err := servernode.Init(dir, cfg); err == nil {
		t.Fatal("Init overwrote an existing profile")
	}
	return dir, cfg
}

func startServer(t *testing.T, dir string) (*servernode.Server, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	srv, err := servernode.Open(ctx, dir, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Close(c)
	})
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return srv, logs
}

func operatorGet(t *testing.T, srv *servernode.Server, dir, path string) (int, []byte) {
	t.Helper()
	tok, err := os.ReadFile(filepath.Join(dir, "operator.token"))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "http://"+srv.OperatorAddr()+path, nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestProfileFilesAreSecret(t *testing.T) {
	dir, _ := profile(t, false)
	for _, name := range []string{"server-identity.json", "server.json"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if os.PathSeparator == '/' && fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v", name, fi.Mode())
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

// A server without a camouflaged listener has no public card and says so; its listeners for
// peers work, and a client can open a session to it (D37, "Готово": a node on localhost).
func TestServerWithoutCoverServesPeers(t *testing.T) {
	dir, cfg := profile(t, false)
	srv, logs := startServer(t, dir)

	code, body := operatorGet(t, srv, dir, "/v1/status")
	if code != http.StatusOK {
		t.Fatalf("status %d %s", code, body)
	}
	var st servernode.StatusInfo
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.PublicCard {
		t.Fatal("a node without a cover has a public card")
	}
	warned := false
	for _, w := range st.Warnings {
		warned = warned || strings.HasPrefix(w, "no-cover")
	}
	if !warned {
		t.Fatalf("no operator warning about the missing cover: %v", st.Warnings)
	}
	if len(st.Listen) != 1 || !strings.Contains(st.Listen[0], "127.0.0.1") {
		t.Fatalf("listen %v", st.Listen)
	}

	// A client connects to the peer card.
	raw, err := srv.PeerCard(context.Background())
	if err != nil {
		t.Fatalf("PeerCard: %v", err)
	}
	card, err := objects.VerifyCard(raw, time.Now())
	if err != nil {
		t.Fatalf("VerifyCard: %v", err)
	}
	client := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	peer, sess, err := client.Session(ctx, node.DeviceID(card.DeviceID), node.DialOpt{Card: card})
	if err != nil {
		t.Fatalf("client Session: %v", err)
	}
	if peer.Device != node.DeviceID(card.DeviceID) || sess == nil {
		t.Fatalf("peer %+v", peer)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body := operatorGet(t, srv, dir, "/v1/status")
		_ = json.Unmarshal(body, &st)
		if st.Sessions >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st.Sessions < 1 {
		t.Fatal("the server does not count the session")
	}
	_ = cfg

	// No key material in the log, no debug listener beside the node's own ports.
	for _, bad := range []string{"mnemonic", "seed", "private", "token"} {
		if strings.Contains(strings.ToLower(logs.String()), bad+"=") {
			t.Errorf("log mentions %q:\n%s", bad, logs)
		}
	}
	tok, _ := os.ReadFile(filepath.Join(dir, "operator.token"))
	if strings.Contains(logs.String(), strings.TrimSpace(string(tok))) {
		t.Error("the operator token is in the log")
	}
}

func TestServerInvitations(t *testing.T) {
	dir, _ := profile(t, true)
	srv, _ := startServer(t, dir)
	tok, _ := os.ReadFile(filepath.Join(dir, "operator.token"))
	post := func(path, body string) (int, []byte) {
		req, _ := http.NewRequest("POST", "http://"+srv.OperatorAddr()+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	code, body := post("/v1/invites", `{"access":"30d"}`)
	if code != http.StatusCreated || !strings.Contains(string(body), "echofog://") {
		t.Fatalf("create invite: %d %s", code, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)
	if len(created.ID) != 16 {
		t.Fatalf("invite id %q", created.ID)
	}
	if code, body := operatorGet(t, srv, dir, "/v1/access"); code != http.StatusOK || !strings.Contains(string(body), created.ID) {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, body := post("/v1/access/"+created.ID+"/extend", `{"by":"7d"}`); code != http.StatusOK {
		t.Fatalf("extend: %d %s", code, body)
	}
	if code, body := post("/v1/rotate-keys", ``); code != http.StatusOK {
		t.Fatalf("rotate-keys: %d %s", code, body)
	}
}

func TestServerWithoutInviteListenerRefusesInvites(t *testing.T) {
	dir, _ := profile(t, false)
	srv, _ := startServer(t, dir)
	tok, _ := os.ReadFile(filepath.Join(dir, "operator.token"))
	req, _ := http.NewRequest("POST", "http://"+srv.OperatorAddr()+"/v1/invites", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// Roles are chosen by the profile: the card says exactly what the config enabled.
func TestRolesComeFromTheConfig(t *testing.T) {
	cfg := servernode.DefaultConfig()
	cfg.Roles = []string{"relay", "exit"}
	cfg.Listen = []servernode.ListenSpec{{Suite: "noise3", Bind: fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t)), Advertise: "127.0.0.1"}}
	dir := filepath.Join(t.TempDir(), "node")
	if _, err := servernode.Init(dir, cfg); err != nil {
		t.Fatal(err)
	}
	srv, _ := startServer(t, dir)
	raw, err := srv.PeerCard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	card, err := objects.VerifyCard(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	flags := card.Body.GetFlags()
	if flags&objects.FlagRelay == 0 || flags&objects.FlagExit == 0 || flags&objects.FlagMailbox != 0 || flags&objects.FlagDHTServer != 0 {
		t.Fatalf("card flags %#x do not match roles relay+exit", flags)
	}
	if card.Role() != objects.RoleServer {
		t.Fatalf("card role %d", card.Role())
	}
}

func TestOpenRefusesBrokenProfile(t *testing.T) {
	dir, _ := profile(t, false)
	if err := os.WriteFile(filepath.Join(dir, "server.json"), []byte(`{"version":1,"listen":[],"unknown":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := servernode.Open(context.Background(), dir, nil); err == nil {
		t.Fatal("Open accepted a broken configuration")
	}
	if _, err := servernode.Open(context.Background(), filepath.Join(t.TempDir(), "none"), nil); err == nil {
		t.Fatal("Open accepted a missing profile")
	}
}

// newClient is a plain desktop-like runtime that dials the server.
func newClient(t *testing.T) *node.Runtime {
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
	rt, err := node.New(node.Config{Account: tw.Account, Device: tw.Device, Cards: builder, Store: st,
		Profile:   node.Profile{Platform: node.PlatformDesktop},
		Country:   func(netip.Addr) (string, bool) { return "lab", true },
		LocalInfo: carrier.LocalInfo{Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}})
	if err != nil {
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
