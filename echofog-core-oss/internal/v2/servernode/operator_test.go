package servernode_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/servernode"
)

const opToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeBackend struct {
	mu       sync.Mutex
	noInvite bool
	minted   []access.MintOptions
	extended []time.Duration
	rotated  int
}

func (b *fakeBackend) Status() servernode.StatusInfo {
	return servernode.StatusInfo{NodeID: "aa", DeviceID: "bb", Roles: []string{"relay"}, Sessions: 3,
		Warnings: []string{"no-cover: no public card"}}
}

func (b *fakeBackend) MintInvite(_ context.Context, o access.MintOptions) (*access.Minted, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.noInvite {
		return nil, servernode.ErrNoInvites
	}
	b.minted = append(b.minted, o)
	return &access.Minted{PassID: access.PassID{1, 2, 3, 4, 5, 6, 7, 8}, Secret: []byte("SECRET-BYTES-0123"),
		Link: "echofog://join/SECRET-LINK-XYZ", LinkExpires: time.Unix(2000000000, 0)}, nil
}

func (b *fakeBackend) ListAccess(context.Context) ([]access.Info, error) {
	return []access.Info{{ID: access.PassID{1, 2, 3, 4, 5, 6, 7, 8}, Kind: access.Invitation, AccessExpires: time.Unix(2000000000, 0)}}, nil
}

func (b *fakeBackend) ExtendAccess(_ context.Context, id access.PassID, d time.Duration) (access.Info, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.extended = append(b.extended, d)
	return access.Info{ID: id, Kind: access.Invitation, AccessExpires: time.Unix(2000000000, 0).Add(d)}, nil
}

func (b *fakeBackend) RotateKeys(context.Context) (*access.Rotation, error) {
	b.mu.Lock()
	b.rotated++
	b.mu.Unlock()
	return &access.Rotation{Reissued: []access.Minted{{PassID: access.PassID{9}, Link: "echofog://join/REISSUED-LINK"}}}, nil
}

type opRig struct {
	h    http.Handler
	b    *fakeBackend
	log  *bytes.Buffer
	now  time.Time
	nowM sync.Mutex
}

func newOpRig(t *testing.T) *opRig {
	t.Helper()
	r := &opRig{b: &fakeBackend{}, log: &bytes.Buffer{}, now: time.Unix(1_800_000_000, 0)}
	r.h = servernode.NewOperator(servernode.OperatorOptions{
		Backend: r.b, Token: opToken,
		Log: slog.New(slog.NewTextHandler(r.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now: func() time.Time { r.nowM.Lock(); defer r.nowM.Unlock(); return r.now },
	})
	return r
}

func (r *opRig) advance(d time.Duration) { r.nowM.Lock(); r.now = r.now.Add(d); r.nowM.Unlock() }

func (r *opRig) do(method, path, token, host, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+goodHost+path, strings.NewReader(body))
	req.Host = host
	req.RemoteAddr = "127.0.0.1:40000"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w
}

const goodHost = "127.0.0.1:8431"

func TestOperatorAuth(t *testing.T) {
	r := newOpRig(t)
	for name, tok := range map[string]string{"none": "", "wrong": strings.Repeat("0", 64), "prefix": opToken[:63], "longer": opToken + "0"} {
		if w := r.do("GET", "/v1/status", tok, goodHost, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s token: %d", name, w.Code)
		}
	}
	if w := r.do("GET", "/v1/status", opToken, goodHost, ""); w.Code != http.StatusOK {
		t.Fatalf("good token: %d %s", w.Code, w.Body)
	}
	// The same answer whatever is wrong with the credentials.
	a, b := r.do("GET", "/v1/status", "", goodHost, ""), r.do("GET", "/v1/status", strings.Repeat("1", 64), goodHost, "")
	if a.Body.String() != b.Body.String() {
		t.Error("missing and wrong tokens are told apart")
	}
	// Other schemes do not count.
	req := httptest.NewRequest("GET", "http://"+goodHost+"/v1/status", nil)
	req.Header.Set("Authorization", "Basic "+opToken)
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("basic scheme: %d", w.Code)
	}
	// A token in the query string is never read.
	if w := r.do("GET", "/v1/status?token="+opToken, "", goodHost, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("token in the query: %d", w.Code)
	}
}

// DNS rebinding: a page on evil.example resolving to 127.0.0.1 sends Host: evil.example.
func TestOperatorHostGuard(t *testing.T) {
	r := newOpRig(t)
	for _, host := range []string{"evil.example", "evil.example:8431", "localhost:8431", "localhost", "0.0.0.0:8431", "192.168.1.5:8431",
		"[::ffff:127.0.0.1]:8431", "127.0.0.1.evil.example:8431", ""} {
		if w := r.do("GET", "/v1/status", opToken, host, ""); w.Code != http.StatusForbidden && w.Code != http.StatusBadRequest {
			t.Errorf("Host %q: %d", host, w.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:8431", "127.9.9.9:80", "[::1]:8431"} {
		if w := r.do("GET", "/v1/status", opToken, host, ""); w.Code != http.StatusOK {
			t.Errorf("Host %q: %d", host, w.Code)
		}
	}
}

func TestOperatorNoDebugEndpoints(t *testing.T) {
	r := newOpRig(t)
	for _, p := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/vars", "/metrics", "/", "/v1"} {
		for _, tok := range []string{"", opToken} {
			if w := r.do("GET", p, tok, goodHost, ""); w.Code == http.StatusOK {
				t.Errorf("%s (token %q) answered 200", p, tok)
			}
		}
	}
	if w := r.do("GET", "/v1/status", opToken, goodHost, ""); w.Code != http.StatusOK {
		t.Fatalf("the status endpoint itself: %d", w.Code)
	}
}

func TestOperatorInvites(t *testing.T) {
	r := newOpRig(t)
	w := r.do("POST", "/v1/invites", opToken, goodHost, `{"access":"30d","link":"7d","max_entries":3}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "SECRET-LINK-XYZ") {
		t.Fatalf("the invite link is not in the answer: %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "SECRET-BYTES") {
		t.Fatal("the raw secret is in the answer")
	}
	o := r.b.minted[0]
	if o.AccessTTL != 30*24*time.Hour || o.LinkTTL != 7*24*time.Hour || o.MaxEntries != 3 {
		t.Fatalf("mint options %+v", o)
	}
	// No body means the defaults of the manager.
	if w := r.do("POST", "/v1/invites", opToken, goodHost, ``); w.Code != http.StatusCreated {
		t.Fatalf("create with defaults: %d %s", w.Code, w.Body)
	}
	for name, body := range map[string]string{
		"bad duration": `{"access":"soon"}`, "too long access": `{"access":"1000d"}`, "too long link": `{"link":"31d"}`,
		"unknown field": `{"access":"30d","admin":true}`, "negative entries": `{"max_entries":-1}`, "not json": `x`,
		"two documents": `{} {}`,
	} {
		if w := r.do("POST", "/v1/invites", opToken, goodHost, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := r.do("POST", "/v1/invites", opToken, goodHost, `{"x":"`+strings.Repeat("a", 5000)+`"}`); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("big body: %d", w.Code)
	}
	r.b.noInvite = true
	if w := r.do("POST", "/v1/invites", opToken, goodHost, ``); w.Code != http.StatusConflict {
		t.Errorf("invites off: %d", w.Code)
	}
	if w := r.do("GET", "/v1/invites", opToken, goodHost, ``); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on invites: %d", w.Code)
	}
}

func TestOperatorExtendListRotate(t *testing.T) {
	r := newOpRig(t)
	if w := r.do("GET", "/v1/access", opToken, goodHost, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "0102030405060708") {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if w := r.do("POST", "/v1/access/0102030405060708/extend", opToken, goodHost, `{"by":"7d"}`); w.Code != http.StatusOK {
		t.Fatalf("extend: %d %s", w.Code, w.Body)
	}
	if len(r.b.extended) != 1 || r.b.extended[0] != 7*24*time.Hour {
		t.Fatalf("extended by %v", r.b.extended)
	}
	for name, p := range map[string]string{"short id": "0102", "not hex": "zzzzzzzzzzzzzzzz", "long": "010203040506070809"} {
		if w := r.do("POST", "/v1/access/"+p+"/extend", opToken, goodHost, `{"by":"7d"}`); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := r.do("POST", "/v1/access/0102030405060708/extend", opToken, goodHost, `{"by":"0d"}`); w.Code != http.StatusBadRequest {
		t.Errorf("zero extension: %d", w.Code)
	}
	w := r.do("POST", "/v1/rotate-keys", opToken, goodHost, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "REISSUED-LINK") || r.b.rotated != 1 {
		t.Fatalf("rotate: %d %s", w.Code, w.Body)
	}
	if w := r.do("GET", "/v1/rotate-keys", opToken, goodHost, ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET rotate: %d", w.Code)
	}
}

func TestOperatorStatusShowsWarnings(t *testing.T) {
	r := newOpRig(t)
	w := r.do("GET", "/v1/status", opToken, goodHost, "")
	var st servernode.StatusInfo
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || st.Sessions != 3 || len(st.Warnings) != 1 {
		t.Fatalf("status %s (%v)", w.Body, err)
	}
}

// Guessing the token is throttled: ten failures from one address lock it out for a minute.
func TestOperatorLockout(t *testing.T) {
	r := newOpRig(t)
	for i := 0; i < 10; i++ {
		r.do("GET", "/v1/status", strings.Repeat("0", 64), goodHost, "")
	}
	if w := r.do("GET", "/v1/status", opToken, goodHost, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("after 10 failures: %d", w.Code)
	}
	r.advance(61 * time.Second)
	if w := r.do("GET", "/v1/status", opToken, goodHost, ""); w.Code != http.StatusOK {
		t.Fatalf("after the lockout: %d", w.Code)
	}
}

// The token and the invite secrets never reach the log.
func TestOperatorLogsNoSecrets(t *testing.T) {
	r := newOpRig(t)
	r.do("GET", "/v1/status", "wrongtoken-123", goodHost, "")
	r.do("POST", "/v1/invites", opToken, goodHost, `{"access":"30d"}`)
	r.do("POST", "/v1/rotate-keys", opToken, goodHost, "")
	for _, secret := range []string{opToken, "wrongtoken-123", "SECRET-LINK-XYZ", "SECRET-BYTES", "REISSUED-LINK"} {
		if strings.Contains(r.log.String(), secret) {
			t.Errorf("the log contains %q:\n%s", secret, r.log)
		}
	}
}

func TestEmptyTokenRefusesEverything(t *testing.T) {
	h := servernode.NewOperator(servernode.OperatorOptions{Backend: &fakeBackend{}, Token: ""})
	req := httptest.NewRequest("GET", "http://"+goodHost+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatal("an empty token opened the API")
	}
}

func TestTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.token")
	a, err := servernode.NewTokenFile(path)
	if err != nil || len(a) != 64 {
		t.Fatalf("NewTokenFile = %q, %v", a, err)
	}
	b, err := servernode.NewTokenFile(path)
	if err != nil || b != a {
		t.Fatalf("second call = %q, %v; the token must persist", b, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("token file mode %v", fi.Mode())
	}
	other, _ := servernode.NewTokenFile(filepath.Join(t.TempDir(), "x"))
	if other == a {
		t.Fatal("two files got the same token")
	}
	// A damaged token file is refused, not replaced silently.
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := servernode.NewTokenFile(path); err == nil {
		t.Fatal("a damaged token file was accepted")
	}
}
