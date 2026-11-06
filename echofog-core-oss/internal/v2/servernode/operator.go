package servernode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
)

// StatusInfo is what GET /v1/status returns.
type StatusInfo struct {
	NodeID     string   `json:"node_id"`
	DeviceID   string   `json:"device_id"`
	Roles      []string `json:"roles"`
	Sessions   int      `json:"sessions"`
	PublicCard bool     `json:"public_card"`
	Warnings   []string `json:"warnings,omitempty"`
	Listen     []string `json:"listen,omitempty"`
}

// ErrNoInvites: invitations are off (no invite listener configured).
var ErrNoInvites = errors.New("servernode: invitations are not configured")

// Backend is the node as the operator API sees it.
type Backend interface {
	Status() StatusInfo
	MintInvite(ctx context.Context, opt access.MintOptions) (*access.Minted, error)
	ListAccess(ctx context.Context) ([]access.Info, error)
	ExtendAccess(ctx context.Context, id access.PassID, d time.Duration) (access.Info, error)
	RotateKeys(ctx context.Context) (*access.Rotation, error)
}

// OperatorOptions configures the operator handler.
type OperatorOptions struct {
	Backend Backend
	// Token is the bearer token; empty refuses everything.
	Token string
	Log   *slog.Logger
	Now   func() time.Time
}

// Limits of the operator API.
const (
	maxBody        = 4096
	failuresToLock = 10
	lockoutFor     = time.Minute
	maxTrackedIPs  = 1024
	maxLinkTTL     = 30 * 24 * time.Hour
	maxEntriesCap  = 10000
)

type operator struct {
	b     Backend
	token [sha256.Size]byte
	has   bool
	log   *slog.Logger
	now   func() time.Time

	mu    sync.Mutex
	fails map[string][]time.Time
	lock  map[string]time.Time
}

// NewOperator returns the operator API handler. Every request must come with a loopback IP
// literal as Host (no DNS rebinding) and a bearer token (never read from the URL). Failures are
// throttled per address. Nothing is logged but method, path and status.
func NewOperator(o OperatorOptions) http.Handler {
	op := &operator{b: o.Backend, log: o.Log, now: o.Now, fails: map[string][]time.Time{}, lock: map[string]time.Time{}}
	if op.log == nil {
		op.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if op.now == nil {
		op.now = time.Now
	}
	if o.Token != "" {
		op.token, op.has = sha256.Sum256([]byte(o.Token)), true
	}
	return op
}

func (o *operator) reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (o *operator) fail(w http.ResponseWriter, code int, msg string) {
	o.reply(w, code, map[string]string{"error": msg})
}

func loopbackHost(host string) bool {
	ap, err := netip.ParseAddrPort(host)
	return err == nil && ap.Addr().IsLoopback() && !ap.Addr().Is4In6()
}

func (o *operator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	code := o.serve(w, r)
	o.log.Debug("operator request", "method", r.Method, "path", r.URL.Path, "status", code)
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }

func (o *operator) serve(rw http.ResponseWriter, r *http.Request) int {
	w := &statusWriter{ResponseWriter: rw, code: http.StatusOK}
	if !loopbackHost(r.Host) {
		o.fail(w, http.StatusForbidden, "forbidden")
		return w.code
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := o.now()
	if o.locked(ip, now) {
		w.Header().Set("Retry-After", "60")
		o.fail(w, http.StatusTooManyRequests, "too many failed attempts")
		return w.code
	}
	if !o.authorised(r) {
		o.recordFailure(ip, now)
		w.Header().Set("WWW-Authenticate", "Bearer")
		o.fail(w, http.StatusUnauthorized, "unauthorized")
		return w.code
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	switch p := r.URL.Path; {
	case p == "/v1/status":
		o.method(w, r, http.MethodGet, o.status)
	case p == "/v1/access":
		o.method(w, r, http.MethodGet, o.list)
	case p == "/v1/invites":
		o.method(w, r, http.MethodPost, o.invite)
	case p == "/v1/rotate-keys":
		o.method(w, r, http.MethodPost, o.rotate)
	case strings.HasPrefix(p, "/v1/access/") && strings.HasSuffix(p, "/extend"):
		o.method(w, r, http.MethodPost, o.extend)
	default:
		o.fail(w, http.StatusNotFound, "not found")
	}
	return w.code
}

func (o *operator) method(w http.ResponseWriter, r *http.Request, want string, h func(http.ResponseWriter, *http.Request)) {
	if r.Method != want {
		w.Header().Set("Allow", want)
		o.fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	h(w, r)
}

func (o *operator) authorised(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !o.has || !strings.HasPrefix(h, prefix) {
		return false
	}
	got := sha256.Sum256([]byte(h[len(prefix):]))
	return subtle.ConstantTimeCompare(got[:], o.token[:]) == 1
}

func (o *operator) locked(ip string, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	until, ok := o.lock[ip]
	if ok && now.Before(until) {
		return true
	}
	if ok {
		delete(o.lock, ip)
		delete(o.fails, ip)
	}
	return false
}

func (o *operator) recordFailure(ip string, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.fails) >= maxTrackedIPs {
		clear(o.fails) // bounded memory; an attacker with that many addresses gains little
		clear(o.lock)
	}
	recent := o.fails[ip][:0:0]
	for _, t := range o.fails[ip] {
		if now.Sub(t) < lockoutFor {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	o.fails[ip] = recent
	if len(recent) >= failuresToLock {
		o.lock[ip] = now.Add(lockoutFor)
	}
}

// readJSON decodes an optional strict JSON body into v.
func readJSON(w http.ResponseWriter, r *http.Request, v any) (int, string) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			return http.StatusRequestEntityTooLarge, "request too large"
		}
		return http.StatusBadRequest, "unreadable request"
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return 0, ""
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return http.StatusBadRequest, "bad request"
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return http.StatusBadRequest, "bad request"
	}
	return 0, ""
}

func (o *operator) status(w http.ResponseWriter, _ *http.Request) {
	o.reply(w, http.StatusOK, o.b.Status())
}

type accessItem struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	State         string    `json:"state"`
	AccessExpires time.Time `json:"access_expires"`
	LinkExpires   time.Time `json:"link_expires,omitempty"`
	Entries       uint32    `json:"entries"`
	MaxEntries    uint32    `json:"max_entries"`
}

func itemOf(i access.Info) accessItem {
	return accessItem{ID: hex.EncodeToString(i.ID[:]), Kind: i.Kind.String(), State: i.State.String(),
		AccessExpires: i.AccessExpires, LinkExpires: i.LinkExpires, Entries: i.Entries, MaxEntries: i.MaxEntries}
}

func (o *operator) backendError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNoInvites) {
		o.fail(w, http.StatusConflict, "invitations are not configured")
		return
	}
	o.log.Warn("operator request failed", "path", r.URL.Path, "err", err)
	o.fail(w, http.StatusInternalServerError, "internal error")
}

func (o *operator) list(w http.ResponseWriter, r *http.Request) {
	infos, err := o.b.ListAccess(r.Context())
	if err != nil {
		o.backendError(w, r, err)
		return
	}
	out := make([]accessItem, 0, len(infos))
	for _, i := range infos {
		out = append(out, itemOf(i))
	}
	o.reply(w, http.StatusOK, out)
}

func (o *operator) invite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Access     string `json:"access"`
		Link       string `json:"link"`
		MaxEntries int64  `json:"max_entries"`
		Share      bool   `json:"share"`
	}
	if code, msg := readJSON(w, r, &req); code != 0 {
		o.fail(w, code, msg)
		return
	}
	opt := access.MintOptions{Share: req.Share}
	if req.MaxEntries < 0 || req.MaxEntries > maxEntriesCap {
		o.fail(w, http.StatusBadRequest, "max_entries out of range")
		return
	}
	opt.MaxEntries = uint32(req.MaxEntries)
	for _, t := range []struct {
		in  string
		out *time.Duration
		max time.Duration
	}{{req.Access, &opt.AccessTTL, access.MaxPersonalTTL}, {req.Link, &opt.LinkTTL, maxLinkTTL}} {
		if t.in == "" {
			continue
		}
		d, err := ParseDuration(t.in)
		if err != nil || d > t.max {
			o.fail(w, http.StatusBadRequest, "bad term")
			return
		}
		*t.out = d
	}
	m, err := o.b.MintInvite(r.Context(), opt)
	if err != nil {
		o.backendError(w, r, err)
		return
	}
	o.reply(w, http.StatusCreated, map[string]any{"id": hex.EncodeToString(m.PassID[:]), "link": m.Link, "link_expires": m.LinkExpires})
}

func (o *operator) extend(w http.ResponseWriter, r *http.Request) {
	idText := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/access/"), "/extend")
	raw, err := hex.DecodeString(idText)
	if err != nil || len(raw) != len(access.PassID{}) {
		o.fail(w, http.StatusBadRequest, "bad id")
		return
	}
	var id access.PassID
	copy(id[:], raw)
	var req struct {
		By string `json:"by"`
	}
	if code, msg := readJSON(w, r, &req); code != 0 {
		o.fail(w, code, msg)
		return
	}
	d, err := ParseDuration(req.By)
	if err != nil || d > access.MaxPersonalTTL {
		o.fail(w, http.StatusBadRequest, "bad term")
		return
	}
	info, err := o.b.ExtendAccess(r.Context(), id, d)
	if err != nil {
		o.backendError(w, r, err)
		return
	}
	o.reply(w, http.StatusOK, itemOf(info))
}

func (o *operator) rotate(w http.ResponseWriter, r *http.Request) {
	rot, err := o.b.RotateKeys(r.Context())
	if err != nil {
		o.backendError(w, r, err)
		return
	}
	type reissued struct {
		ID   string `json:"id"`
		Link string `json:"link"`
	}
	out := make([]reissued, 0, len(rot.Reissued))
	for _, m := range rot.Reissued {
		out = append(out, reissued{ID: hex.EncodeToString(m.PassID[:]), Link: m.Link})
	}
	o.reply(w, http.StatusOK, map[string]any{"reissued": out})
}

// NewTokenFile returns the token stored at path, creating it (random, mode 0600) when missing. A
// damaged file is an error, never silently replaced: replacing it would lock the operator's
// scripts out without a word.
func NewTokenFile(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		tok := strings.TrimSpace(string(b))
		if raw, derr := hex.DecodeString(tok); derr != nil || len(raw) != 32 || tok != strings.ToLower(tok) {
			return "", fmt.Errorf("servernode: token file %s is damaged; delete it to make a new token", path)
		}
		return tok, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		_ = f.Close()
		return "", err
	}
	return tok, f.Close()
}
