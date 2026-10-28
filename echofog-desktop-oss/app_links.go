package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// EchoFog links (echofog://join?…, echofog://relay/…) open the app. The OS starts a new process
// with the link as its argument; when an EchoFog window already runs, that process hands the link
// over and quits, and the window shows its "open link" dialog. Several copies may run side by side
// (two profiles, two builds): the one started last receives the links.

const linkScheme = "echofog://"

var (
	startLinkMu sync.Mutex
	startLink   string // the link this process was started with, until the UI takes it
)

// linkFromArgs finds an EchoFog link among the command line arguments.
func linkFromArgs(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(a)), linkScheme) {
			return strings.TrimSpace(a)
		}
	}
	return ""
}

// TakeStartLink returns the link the app was started with, once ("" when there is none).
func (a *App) TakeStartLink() string {
	startLinkMu.Lock()
	defer startLinkMu.Unlock()
	l := startLink
	startLink = ""
	return l
}

// JoinInvite joins the node an invitation link names (a hidden node lets us in with it) and
// returns its peer ID.
func (a *App) JoinInvite(link string) (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errNodeNotStarted
	}
	return nodeAPI.JoinInvite(link)
}

// linkHandoffFile is where the running window says how to reach it.
func linkHandoffFile() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "EchoFog", "link-handoff")
}

// handOffLink gives the link to a running EchoFog window; false when none answers.
func handOffLink(link string) bool {
	data, err := os.ReadFile(linkHandoffFile())
	if err != nil {
		return false
	}
	addr, token, ok := strings.Cut(strings.TrimSpace(string(data)), " ")
	if !ok {
		return false
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/link", strings.NewReader(link))
	if err != nil {
		return false
	}
	req.Header.Set("X-EchoFog-Token", token)
	res, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	return res.StatusCode == http.StatusNoContent
}

// serveLinkHandoff lets later processes give this window their links. It listens on loopback only
// and answers only with the token written next to the address.
func (a *App) serveLinkHandoff(ctx context.Context) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		_ = ln.Close()
		return
	}
	token := hex.EncodeToString(raw)
	file := linkHandoffFile()
	_ = os.MkdirAll(filepath.Dir(file), 0o700)
	if err := os.WriteFile(file, []byte(ln.Addr().String()+" "+token), 0o600); err != nil {
		_ = ln.Close()
		return
	}
	srv := &http.Server{ReadHeaderTimeout: 3 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/link" || r.Header.Get("X-EchoFog-Token") != token {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<10))
		link := linkFromArgs([]string{string(body)})
		if link == "" {
			http.Error(w, "not an EchoFog link", http.StatusBadRequest)
			return
		}
		runtime.WindowUnminimise(ctx)
		runtime.WindowShow(ctx)
		safeEventsEmit(ctx, "link:open", link)
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		// only remove the file if it still points here (a later window may have taken over)
		if data, err := os.ReadFile(file); err == nil && strings.HasPrefix(string(data), ln.Addr().String()+" ") {
			_ = os.Remove(file)
		}
	}()
}
