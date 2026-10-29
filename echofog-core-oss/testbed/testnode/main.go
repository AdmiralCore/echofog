// Command testnode is a node for the Docker test bed. It runs the same code as echofog-node and adds
// a small HTTP control interface, so that the scenario runner can send messages, build groups and
// read what arrived. It is only meant for the test bed and never ships.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	api "github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/node"
)

type testNode struct {
	name    string
	started time.Time
	n       *node.Node
	api     *api.NodeApi
	rx      *callRx
}

func main() {
	var (
		name      = flag.String("name", "node", "name in logs and status")
		dataDir   = flag.String("data-dir", "/data", "where the database lives (kept between restarts)")
		listen    = flag.String("listen", ":4242", "P2P listen address")
		bootstrap = flag.String("bootstrap", "", "comma-separated host:port/peerid")
		httpAddr  = flag.String("http", ":8080", "control interface")
		stun      = flag.String("stun", "203.0.113.11:3478", "STUN server; the lab has no Internet, so this stays unanswered")
		transport = flag.String("transport", "", "auto, quic or tcp")
		signaling = flag.Bool("signaling", os.Getenv("SIGNALING") != "", "signaling only: DHT and hole punching help, no relayed traffic")
		server    = flag.Bool("server", os.Getenv("SERVER") != "", "server role, as on a VPS: relay, DHT and exit, no chat")
		coverSNI  = flag.String("cover-sni", os.Getenv("TLS_SERVER_NAME"), "SNI the TCP fallback mimics (with -cover-addr: TCP listener on the P2P port)")
		coverAddr = flag.String("cover-addr", os.Getenv("CAMOUFLAGE_ADDR"), "the real site probes of the TCP fallback are sent to")
		exit      = flag.Bool("exit", os.Getenv("EXIT") != "", "share the connection as a free exit")
		web       = flag.String("web", "", "run only the lab's web server on this address, no node")
		access    = flag.String("access", os.Getenv("ACCESS"), "public, hidden or isolated")
		pubAddr   = flag.String("public-addr", os.Getenv("PUBLIC_ADDR"), "address put into the invitation link")
	)
	flag.Parse()
	if *web != "" {
		serveWeb(*web)
		return
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	pass := password(*dataDir)

	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = *listen
	cfg.Storage.DatabasePath = filepath.Join(*dataDir, "db")
	cfg.Network.PortMapping = false
	cfg.Network.STUNServers = []string{*stun}
	if *transport != "" {
		cfg.Network.Transport = *transport
	}
	if *signaling {
		cfg.Relay.Server.Enabled = true
		cfg.Relay.Server.SignalingOnly = true
	}
	if *server {
		cfg.Role = config.RoleServer
		cfg.Relay.Server.Enabled = true
	}
	if *coverSNI != "" && *coverAddr != "" {
		cfg.AntiDPI.TLSServerName, cfg.AntiDPI.CamouflageAddr = *coverSNI, *coverAddr
		cfg.Network.TCPListenAddr = *listen
	}
	if g := os.Getenv("GUARD_MODE"); g != "" {
		cfg.AntiDPI.GuardMode = g
	}
	if g := os.Getenv("TCP_ROTATE"); g != "" {
		cfg.AntiDPI.TCPRotate = g
	}
	if *access != "" {
		cfg.Access.Mode = *access
	}
	if *pubAddr != "" {
		cfg.Access.PublicAddrs = []string{*pubAddr}
	}
	// a lab node knows only the nodes it is given: never the built-in seeds, or a test run on a
	// developer's machine joins the real network from their home address
	cfg.Network.BootstrapNodes = nil
	for _, b := range strings.Split(*bootstrap, ",") {
		if b = strings.TrimSpace(b); b != "" {
			cfg.Network.BootstrapNodes = append(cfg.Network.BootstrapNodes, b)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := node.NewNode(ctx, cfg, *name)
	if n.IsRegistered() {
		if err := n.Login([]byte(pass)); err != nil {
			log.Fatalf("login: %v", err)
		}
	} else if _, err := n.Register([]byte(pass), nil); err != nil {
		log.Fatalf("register: %v", err)
	}
	a := api.NewNodeApi(n)
	if err := a.StartNode(); err != nil { // also starts the group service
		log.Fatalf("start: %v", err)
	}

	t := &testNode{name: *name, started: time.Now(), n: n, api: a}
	if *exit {
		if err := t.startExit(); err != nil {
			log.Fatalf("exit: %v", err)
		}
	}
	srv := &http.Server{Addr: *httpAddr, Handler: t.routes()}
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()
	fmt.Printf("testnode %s ready: id=%s\n", *name, n.ID)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	_ = srv.Shutdown(context.Background())
	n.Stop()
}

// password is generated once and kept next to the database, so that a restart logs in unattended.
func password(dir string) string {
	file := filepath.Join(dir, ".pw")
	if b, err := os.ReadFile(file); err == nil && len(b) > 0 {
		return string(b)
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	p := hex.EncodeToString(raw)
	_ = os.WriteFile(file, []byte(p), 0o600)
	return p
}

func (t *testNode) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", t.status)
	mux.HandleFunc("/send", t.send)
	mux.HandleFunc("/history", t.history)
	mux.HandleFunc("/pending", t.pending)
	mux.HandleFunc("/group/create", t.groupCreate)
	mux.HandleFunc("/group/list", t.groupList)
	mux.HandleFunc("/group/invite", t.groupInvite)
	mux.HandleFunc("/group/join", t.groupJoin)
	mux.HandleFunc("/group/send", t.groupSend)
	mux.HandleFunc("/group/messages", t.groupMessages)
	mux.HandleFunc("/group/members", t.groupMembers)
	t.vpnRoutes(mux)
	t.callRoutes(mux)
	t.guardRoutes(mux)
	mux.HandleFunc("/access", t.accessStatus)
	mux.HandleFunc("/access/kick", t.accessKick)
	mux.HandleFunc("/exits", t.exits)
	return mux
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func body(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

func (t *testNode) status(w http.ResponseWriter, _ *http.Request) {
	peers := t.api.GetPeers()
	ids := make([]string, 0, len(peers))
	for _, p := range peers {
		ids = append(ids, p.PeerID)
	}
	reply(w, map[string]any{
		"name": t.name, "id": t.api.GetMyID(), "pubKey": t.api.GetMyPubKey(),
		"peers": len(peers), "peerIds": ids, "uptimeSec": int(time.Since(t.started).Seconds()),
	}, nil)
}

func (t *testNode) send(w http.ResponseWriter, r *http.Request) {
	var in struct{ To, Text string }
	if err := body(r, &in); err != nil {
		reply(w, nil, err)
		return
	}
	reply(w, map[string]bool{"ok": true}, t.api.SendMessage(in.To, in.Text))
}

// history lists the messages of one conversation, newest last; what is kept on disk, so it
// survives a restart of the node.
func (t *testNode) history(w http.ResponseWriter, r *http.Request) {
	msgs, err := t.api.GetMessages(r.URL.Query().Get("peer"), 5000, 0)
	reply(w, msgs, err)
}

func (t *testNode) pending(w http.ResponseWriter, r *http.Request) {
	msgs, err := t.api.GetMessages(r.URL.Query().Get("peer"), 5000, 0)
	n := 0
	for _, m := range msgs {
		if m.Pending {
			n++
		}
	}
	reply(w, map[string]int{"pending": n}, err)
}

func (t *testNode) groupCreate(w http.ResponseWriter, r *http.Request) {
	var in struct{ Title, Kind string }
	if err := body(r, &in); err != nil {
		reply(w, nil, err)
		return
	}
	g, err := t.api.CreateGroup(in.Title, "", in.Kind)
	reply(w, g, err)
}

func (t *testNode) groupList(w http.ResponseWriter, _ *http.Request) {
	g, err := t.api.ListGroups()
	reply(w, g, err)
}

func (t *testNode) groupInvite(w http.ResponseWriter, r *http.Request) {
	inv, err := t.api.GetGroupInvite(r.URL.Query().Get("id"))
	reply(w, map[string]string{"invite": inv}, err)
}

func (t *testNode) groupJoin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Invite string }
	if err := body(r, &in); err != nil {
		reply(w, nil, err)
		return
	}
	g, err := t.api.JoinGroup(in.Invite)
	reply(w, g, err)
}

func (t *testNode) groupSend(w http.ResponseWriter, r *http.Request) {
	var in struct{ ID, Text string }
	if err := body(r, &in); err != nil {
		reply(w, nil, err)
		return
	}
	m, err := t.api.SendGroupMessage(in.ID, in.Text)
	reply(w, m, err)
}

func (t *testNode) groupMessages(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	m, err := t.api.GetGroupMessages(r.URL.Query().Get("id"), limit)
	reply(w, m, err)
}

func (t *testNode) groupMembers(w http.ResponseWriter, r *http.Request) {
	m, err := t.api.ListGroupMembers(r.URL.Query().Get("id"))
	reply(w, m, err)
}
