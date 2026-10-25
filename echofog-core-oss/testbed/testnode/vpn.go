package main

// The VPN side of the test node: an exit that shares its connection, a client that goes through
// one, a web server standing in for the Internet, and the numbers a soak run watches.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	api "github.com/echofog/echofog-core/pkg/api/node"
)

// socksAddr is where the client's proxy listens inside its container.
const socksAddr = "127.0.0.1:1080"

func (t *testNode) vpnRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/exit/start", t.exitStart)
	mux.HandleFunc("/exit/status", t.exitStatus)
	mux.HandleFunc("/vpn/start", t.vpnStart)
	mux.HandleFunc("/vpn/stop", t.vpnStop)
	mux.HandleFunc("/vpn/status", t.vpnStatus)
	mux.HandleFunc("/vpn/fetch", t.vpnFetch)
	mux.HandleFunc("/vpn/burst", t.vpnBurst)
	mux.HandleFunc("/dial", t.dial)
	mux.HandleFunc("/metrics", t.metrics)
	mux.HandleFunc("/invite", t.invite)
	mux.HandleFunc("/call/invite", t.callInvite)
}

// callInvite places a call: the invitation message and what it waits for.
func (t *testNode) callInvite(w http.ResponseWriter, r *http.Request) {
	var in struct{ To string }
	if err := body(r, &in); err != nil {
		reply(w, nil, err)
		return
	}
	inv, err := t.api.InviteToCall(in.To)
	reply(w, inv, err)
}

// startExit shares this node's connection for free. The lab's "Internet" is a documentation range
// an exit would refuse, so private and reserved targets are allowed here, and only here.
func (t *testNode) startExit() error {
	t.n.ExitService.AllowPrivateTargets(true)
	if err := t.api.StartSharing(api.SharingConfig{UploadKbps: 100_000}); err != nil {
		return err
	}
	log.Printf("testnode %s: sharing its connection as a free exit", t.name)
	return nil
}

func (t *testNode) exitStart(w http.ResponseWriter, _ *http.Request) {
	reply(w, map[string]bool{"ok": true}, t.startExit())
}

func (t *testNode) exitStatus(w http.ResponseWriter, _ *http.Request) {
	acc, ref, bytes := t.n.ExitService.Stats()
	reply(w, map[string]any{
		"sharing": t.api.GetSharingStatus().Enabled, "sessions": acc, "refused": ref, "bytes": bytes,
		"refusedBy": t.n.ExitService.RefusedByReason(), "streams": t.n.ExitService.Streams(),
	}, nil)
}

func (t *testNode) vpnStart(w http.ResponseWriter, r *http.Request) {
	var in struct{ Target string }
	if err := body(r, &in); err != nil {
		reply(w, nil, err)
		return
	}
	start := time.Now()
	err := t.api.StartVPNVia(in.Target)
	log.Printf("testnode %s: vpn start via %.24s: %v (%s)", t.name, in.Target, err, time.Since(start).Round(time.Millisecond))
	reply(w, map[string]any{"ok": err == nil, "ms": time.Since(start).Milliseconds()}, err)
}

func (t *testNode) vpnStop(w http.ResponseWriter, _ *http.Request) {
	reply(w, map[string]bool{"ok": true}, t.api.StopVPN())
}

func (t *testNode) vpnStatus(w http.ResponseWriter, _ *http.Request) {
	reply(w, t.api.GetVPNStatus(), nil)
}

// fetchResult is one download through the proxy.
type fetchResult struct {
	OK     bool   `json:"ok"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Want   string `json:"want"`
	MS     int64  `json:"ms"`
	Error  string `json:"error,omitempty"`
}

// vpnFetch downloads url through the local proxy and checks the body against the hash the web
// server announced.
func (t *testNode) vpnFetch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	timeout, _ := time.ParseDuration(q.Get("timeout"))
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	res := fetchVia(q.Get("url"), timeout)
	if !res.OK {
		log.Printf("testnode %s: fetch %s failed after %d ms: %s", t.name, q.Get("url"), res.MS, res.Error)
	}
	reply(w, res, nil)
}

// vpnBurst runs n downloads at once and sums them up: the load an exit gets from a browser.
func (t *testNode) vpnBurst(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	n, _ := strconv.Atoi(q.Get("n"))
	n = max(1, min(n, 512))
	timeout, _ := time.ParseDuration(q.Get("timeout"))
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	url := q.Get("url")
	start := time.Now()
	var mu sync.Mutex
	out := map[string]any{}
	ok, bad, total := 0, 0, int64(0)
	errs := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := fetchVia(strings.ReplaceAll(url, "{i}", strconv.Itoa(i)), timeout)
			mu.Lock()
			defer mu.Unlock()
			if res.OK {
				ok++
				total += res.Bytes
			} else {
				bad++
				errs[shortErr(res.Error)]++
			}
		}(i)
	}
	wg.Wait()
	secs := time.Since(start).Seconds()
	out["ok"], out["failed"], out["bytes"], out["seconds"] = ok, bad, total, secs
	out["mbps"] = float64(total) * 8 / 1e6 / max(secs, 0.001)
	out["errors"] = errs
	if bad > 0 {
		log.Printf("testnode %s: burst of %d: %d failed %v", t.name, n, bad, errs)
	}
	reply(w, out, nil)
}

func shortErr(s string) string {
	if i := strings.LastIndex(s, ": "); i >= 0 && len(s)-i < 60 {
		return s[i+2:]
	}
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

func fetchVia(url string, timeout time.Duration) fetchResult {
	start := time.Now()
	res := fetchResult{}
	fail := func(err error) fetchResult {
		res.MS, res.Error = time.Since(start).Milliseconds(), err.Error()
		return res
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tr := &http.Transport{
		DialContext:       func(ctx context.Context, _, addr string) (net.Conn, error) { return socksDial(ctx, socksAddr, addr) },
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fail(err)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fail(fmt.Errorf("http %d", resp.StatusCode))
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	res.Bytes = n
	if err != nil {
		return fail(err)
	}
	res.SHA256 = hex.EncodeToString(h.Sum(nil))
	res.Want = resp.Header.Get("X-Sha256")
	res.MS = time.Since(start).Milliseconds()
	if res.Want != "" && res.Want != res.SHA256 {
		res.Error = "body differs from what the server sent"
		return res
	}
	res.OK = true
	return res
}

// socksDial opens a CONNECT through a SOCKS5 proxy without authentication.
func socksDial(ctx context.Context, proxy, target string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", proxy)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	fail := func(err error) (net.Conn, error) { c.Close(); return nil, err }
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		return fail(err)
	}
	var hello [2]byte
	if _, err := io.ReadFull(c, hello[:]); err != nil || hello[1] != 0 {
		return fail(fmt.Errorf("socks hello: %v", err))
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fail(err)
	}
	port, _ := strconv.Atoi(portStr)
	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host).To4(); ip != nil {
		req = append(append(req, 1), ip...)
	} else {
		req = append(append(req, 3, byte(len(host))), host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		return fail(err)
	}
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return fail(fmt.Errorf("socks reply: %w", err))
	}
	if head[1] != 0 {
		return fail(fmt.Errorf("socks refused (code %d)", head[1]))
	}
	skip := 0
	switch head[3] {
	case 1:
		skip = 4
	case 4:
		skip = 16
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return fail(err)
		}
		skip = int(l[0])
	}
	if _, err := io.ReadFull(c, make([]byte, skip+2)); err != nil {
		return fail(err)
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// dial connects to addr ("host:port/<peer id>"), as a user pasting a node address does. With a
// made-up peer ID it is a failed handshake, as an old app with an outdated key makes.
func (t *testNode) dial(w http.ResponseWriter, r *http.Request) {
	var in struct{ Addr string }
	if err := body(r, &in); err != nil {
		reply(w, nil, err)
		return
	}
	err := t.api.ConnectToPeer(in.Addr)
	reply(w, map[string]any{"ok": err == nil, "error": errText(err)}, nil)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// invite is this node's join address, as its link would carry it.
func (t *testNode) invite(w http.ResponseWriter, _ *http.Request) {
	reply(w, map[string]string{"id": t.api.GetMyID()}, nil)
}

// metrics are what a soak run graphs: goroutines, memory and open files only grow on a leak.
func (t *testNode) metrics(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("gc") != "" {
		// what is still held after a collection; without it an idle node shows garbage that no
		// allocation has made the collector look at yet
		runtime.GC()
		debug.FreeOSMemory()
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fds := -1
	if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
		fds = len(ents)
	}
	out := map[string]any{
		"goroutines": runtime.NumGoroutine(), "heapMB": float64(ms.HeapInuse) / (1 << 20),
		"sysMB": float64(ms.Sys) / (1 << 20), "fds": fds, "peers": len(t.api.GetPeers()),
		"uptimeSec": int(time.Since(t.started).Seconds()),
	}
	if t.n.ExitService != nil && t.n.ExitService.Enabled() {
		acc, ref, bytes := t.n.ExitService.Stats()
		out["exitSessions"], out["exitRefused"], out["exitBytes"], out["exitStreams"] = acc, ref, bytes, t.n.ExitService.Streams()
	}
	if rs := t.n.GetRelayServer(); rs != nil {
		st := rs.Stats()
		out["relayCircuits"], out["relayStored"], out["relayForwarded"] = st.ActiveCircuits, st.Stored, st.Forwarded
	}
	v := t.api.GetVPNStatus()
	if v.Running {
		out["vpnState"], out["vpnRx"], out["vpnSwitches"] = v.ExitState, v.BytesRx, v.Switches
	}
	reply(w, out, nil)
}

// ---------------------------------------------------------------------------- the "Internet"

// serveWeb is the web server of the lab: /blob?size=N&seed=S answers N bytes of a stream fixed by
// S, with its SHA-256 in X-Sha256, so a client can tell a whole download from a damaged one.
func serveWeb(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pong") })
	mux.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		size, _ := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
		size = max(0, min(size, 1<<30))
		seed := r.URL.Query().Get("seed")
		data := blob(seed, size)
		sum := sha256.Sum256(data)
		w.Header().Set("X-Sha256", hex.EncodeToString(sum[:]))
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		_, _ = w.Write(data)
	})
	// HTTPS on 443 as well: a node's TCP fallback mimics this site and sends probes to it, and a
	// plain HTTPS visit to it is the control of the censor scenario
	go func() {
		srv := &http.Server{Addr: ":443", Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{selfSigned()}}}
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("https: %v", err)
		}
	}()
	log.Printf("web server on %s and :443", addr)
	if err := http.ListenAndServe(addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// selfSigned is the certificate of the lab's cover site.
func selfSigned() tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cover.lab"},
		DNSNames: []string{"cover.lab"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

var blobCache sync.Map // seed/size -> []byte

func blob(seed string, size int64) []byte {
	key := seed + "/" + strconv.FormatInt(size, 10)
	if v, ok := blobCache.Load(key); ok {
		return v.([]byte)
	}
	out := make([]byte, 0, size)
	block := sha256.Sum256([]byte(seed))
	for int64(len(out)) < size {
		block = sha256.Sum256(block[:])
		out = append(out, block[:]...)
	}
	out = out[:size]
	if size <= 64<<20 {
		blobCache.Store(key, out)
	}
	return out
}

// accessStatus is the node's access mode, its invitation link and who joined with it.
func (t *testNode) accessStatus(w http.ResponseWriter, _ *http.Request) {
	st, err := t.api.GetAccessStatus()
	reply(w, st, err)
}

// accessKick takes a person's access away, as the owner of a hidden node does in the app.
func (t *testNode) accessKick(w http.ResponseWriter, r *http.Request) {
	var in struct{ Peer string }
	if err := body(r, &in); err != nil {
		reply(w, nil, err)
		return
	}
	reply(w, map[string]bool{"ok": true}, t.api.RemoveJoinedPeer(in.Peer))
}

// exits is the exit list the app shows, after looking the network over again.
func (t *testNode) exits(w http.ResponseWriter, _ *http.Request) {
	_, _ = t.api.RefreshExitNodes()
	ids := []string{}
	for _, e := range t.api.ListExitNodes() {
		ids = append(ids, e.PeerID)
	}
	reply(w, ids, nil)
}
