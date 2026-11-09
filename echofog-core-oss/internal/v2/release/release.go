// Package release keeps the release manifest of a node (objects.md §9, versioning.md §5): the key
// set and the last applied manifest persisted in the store, the sources that offer manifests (DHT,
// pex/2, the download page), the version policy of the handshake, and the "update available"
// events. The checks themselves live in objects.ReleaseState; this package never repeats them.
//
// Nothing here downloads or installs an artifact: the manifest is information for the user (R5).
//
// # Storage (namespace Namespace)
//
//	"v"                         schema version, one byte (1)
//	"m/" ‖ u64be(released_at)   the raw release/2 object, one record per applied manifest
//
// The chain is stored whole because the key set is "the compiled set plus next_keys minus
// revoked_keys of every applied manifest in order" and the effective min_supported is clipped
// against the previous manifest. On start the chain is replayed through a fresh
// objects.ReleaseState, each manifest at its own released_at, so replay does not depend on the
// clock and does not emit events. A chain that no longer verifies under the compiled keys (another
// build) is not trusted: the replay stops at the first failure.
//
// Rollback: a manifest not newer than the last applied one is refused, also after a restart.
// A build that does not know the schema version byte ignores the chain and starts clean.
package release

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Namespace is the store namespace of the manifest chain.
const Namespace = "v2/release"

// Limits.
const (
	// MaxManifestSize is the largest release object that is parsed (registries.md §9: 20480).
	MaxManifestSize = 20480
	// MaxChain is the number of applied manifests a profile keeps (a rotation needs the chain).
	MaxChain = 256

	schemaVersion = 1
	maxRedirects  = 3
)

var (
	versionKey = []byte("v")
	chainPref  = []byte("m/")
)

// Errors.
var (
	// ErrTooLarge reports a manifest above MaxManifestSize.
	ErrTooLarge = errors.New("release: manifest too large")
	// ErrStale reports a manifest that is not newer than the applied one (rollback).
	ErrStale = errors.New("release: not newer than the applied manifest")
	// ErrChainFull reports that the profile already keeps MaxChain manifests.
	ErrChainFull = errors.New("release: manifest chain is full")
	// ErrChainUnavailable reports that the stored chain could not be read or verified: the key set
	// is unknown, so the manager refuses manifests (fail closed) until Reset.
	ErrChainUnavailable = errors.New("release: stored manifest chain unavailable")
	// ErrSource reports a bad download source (not https, no client, bad status).
	ErrSource = errors.New("release: bad source")
)

// Clock is the node's corrected clock (AD13).
type Clock interface{ Now() time.Time }

// Event reports an applied manifest that says the node's own version is outdated.
type Event struct {
	Version  string
	NotesURL string
	At       time.Time
	// Components lists the components of the node whose status is not up to date, by name.
	Components []ComponentStatus
	// Required is true when any component is below min_supported.
	Required bool
}

// ComponentStatus is the update status of one component of the node.
type ComponentStatus struct {
	Component string
	Own       uint32
	Status    objects.UpdateStatus
}

// Config configures a Manager.
type Config struct {
	// Keys are the release public keys compiled into the build. Required.
	Keys []keys.AKey
	// KV is the persistent namespace (Namespace). Nil keeps the state in memory only.
	KV store.KV
	// Clock is the corrected clock. Required.
	Clock Clock
	// Own maps a component name to the version this node runs ("proto": 200).
	Own map[string]uint32
	// OnEvent receives "update available" events. It is called without internal locks and must not
	// block for long.
	OnEvent func(Event)
	// OnBootstrapList receives the signed official entry list carried by an applied manifest
	// (Release.bootstrap_list, T5.6). It is called without internal locks after the manifest is
	// applied, and must not block for long.
	OnBootstrapList func([]byte)
	// Log receives diagnostics. Nil is silent.
	Log *slog.Logger
}

// Manager is the release state of one node. It is safe for concurrent use.
type Manager struct {
	cfg   Config
	state atomic.Pointer[objects.ReleaseState] // replaced only by Reset

	mu       sync.Mutex // serialises Offer, Check and Reset; guards the fields below
	latest   []byte
	chainLen int
	// broken is set when the stored chain could not be read or verified. The key set is then
	// unknown (a key the chain revoked would count again), so Offer and Check refuse (L2).
	broken error
}

// Open creates a Manager and replays the persisted chain.
func Open(cfg Config) (*Manager, error) {
	if cfg.Clock == nil {
		return nil, errors.New("release: a clock is required")
	}
	st, err := objects.NewReleaseState(cfg.Keys)
	if err != nil {
		return nil, fmt.Errorf("release: compiled keys: %w", err)
	}
	m := &Manager{cfg: cfg}
	m.state.Store(st)
	m.replay()
	return m, nil
}

func (m *Manager) warn(msg string, args ...any) {
	if m.cfg.Log != nil {
		m.cfg.Log.Warn(msg, args...)
	}
}

func chainKey(releasedAt uint64) []byte {
	k := make([]byte, len(chainPref)+8)
	copy(k, chainPref)
	binary.BigEndian.PutUint64(k[len(chainPref):], releasedAt)
	return k
}

// lost marks the stored chain unavailable. The state keeps whatever was replayed before the
// failure: its policies still serve the handshake, but no new manifest is taken on a key set that
// may have lost a revocation.
func (m *Manager) lost(format string, args ...any) {
	m.broken = fmt.Errorf("%w: %s", ErrChainUnavailable, fmt.Sprintf(format, args...))
	m.warn("release: stored chain unavailable; manifests are refused until Reset", "err", m.broken)
}

func (m *Manager) replay() {
	kv := m.cfg.KV
	if kv == nil {
		return
	}
	v, err := kv.Get(versionKey)
	if errors.Is(err, store.ErrNotFound) {
		// A fresh profile has no records either; records without a version are a damaged chain.
		rest, serr := kv.Scan(chainPref, 1)
		switch {
		case serr != nil:
			m.lost("reading the chain: %v", serr)
		case len(rest) > 0:
			m.lost("records without a schema version")
		}
		return
	}
	if err != nil {
		m.lost("reading the schema version: %v", err)
		return
	}
	if len(v) != 1 || v[0] != schemaVersion {
		m.lost("unknown schema version")
		return
	}
	entries, err := kv.Scan(chainPref, MaxChain+1)
	if err != nil {
		m.lost("reading the chain: %v", err)
		return
	}
	st := m.state.Load()
	for _, e := range entries {
		if len(e.Key) != len(chainPref)+8 || len(e.Value) > MaxManifestSize {
			m.lost("bad record")
			return
		}
		at := time.Unix(int64(binary.BigEndian.Uint64(e.Key[len(chainPref):])), 0)
		if _, err := st.Apply(e.Value, at); err != nil {
			m.lost("a stored manifest does not verify: %v", err)
			return
		}
		m.latest = append(m.latest[:0], e.Value...)
		m.chainLen++
	}
}

// Check verifies a release object against the current state without applying it. It is the DHT
// server's ReleaseCheck: the compiled keys and the threshold decide what a node stores.
//
// The manifest the node runs on is vouched for as well: "newer than the last applied" is a rule for
// Offer, not for storing the current release (M1). It still has to be a valid, unexpired release
// object.
func (m *Manager) Check(raw []byte, now time.Time) error {
	if len(raw) == 0 || len(raw) > MaxManifestSize {
		return ErrTooLarge
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.broken != nil {
		return m.broken
	}
	if m.latest != nil && bytes.Equal(raw, m.latest) {
		e, err := objects.Verify(raw, now)
		if err != nil {
			return err
		}
		if e.Label != objects.LabelRelease {
			return fmt.Errorf("release: label %q", e.Label)
		}
		return nil
	}
	_, err := m.state.Load().Check(raw, now)
	return err
}

// Offer applies a release object from any source (DHT, pex, the download page). It returns true
// when the object became the current manifest. The same object again applies nothing; an object
// that is not newer is ErrStale. The object is persisted before it is applied, so a failing store
// leaves the state unchanged.
func (m *Manager) Offer(raw []byte) (bool, error) {
	if len(raw) == 0 {
		return false, errors.New("release: empty manifest")
	}
	if len(raw) > MaxManifestSize {
		return false, ErrTooLarge
	}
	m.mu.Lock()
	if m.broken != nil {
		err := m.broken
		m.mu.Unlock()
		return false, err
	}
	if bytes.Equal(raw, m.latest) {
		m.mu.Unlock()
		return false, nil
	}
	now := m.cfg.Clock.Now()
	mf, err := m.state.Load().Check(raw, now)
	if err != nil {
		m.mu.Unlock()
		if m.stale(raw, now) {
			return false, fmt.Errorf("%w: %v", ErrStale, err)
		}
		return false, err
	}
	if m.chainLen >= MaxChain {
		m.mu.Unlock()
		return false, ErrChainFull
	}
	if kv := m.cfg.KV; kv != nil {
		if m.chainLen == 0 {
			if err := kv.Put(versionKey, []byte{schemaVersion}); err != nil {
				m.mu.Unlock()
				return false, fmt.Errorf("release: persist: %w", err)
			}
		}
		if err := kv.Put(chainKey(mf.Release.GetReleasedAt()), raw); err != nil {
			m.mu.Unlock()
			return false, fmt.Errorf("release: persist: %w", err)
		}
	}
	applied, err := m.state.Load().Apply(raw, now)
	if err != nil {
		// Check just passed under the same lock; if this happens the record must not stay.
		if kv := m.cfg.KV; kv != nil {
			_ = kv.Delete(chainKey(mf.Release.GetReleasedAt()))
		}
		m.mu.Unlock()
		return false, err
	}
	m.latest = append([]byte(nil), raw...)
	m.chainLen++
	ev, notify := m.eventFor(applied, now)
	m.mu.Unlock()
	if notify && m.cfg.OnEvent != nil {
		m.cfg.OnEvent(ev)
	}
	return true, nil
}

// stale reports whether raw is a well-formed signed object whose seq (= released_at) is not newer
// than the applied manifest. It only picks the error to return.
func (m *Manager) stale(raw []byte, now time.Time) bool {
	last := m.state.Load().Last()
	if last == nil {
		return false
	}
	e, err := objects.Verify(raw, now)
	return err == nil && e.Seq <= last.Release.GetReleasedAt()
}

func (m *Manager) eventFor(mf *objects.Manifest, now time.Time) (Event, bool) {
	ev := Event{Version: mf.Release.GetVersion(), NotesURL: mf.Release.GetNotesUrl(), At: now}
	names := make([]string, 0, len(m.cfg.Own))
	for c := range m.cfg.Own {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		own := m.cfg.Own[c]
		st := m.state.Load().Status(c, own, now)
		if st == objects.UpToDate {
			continue
		}
		ev.Components = append(ev.Components, ComponentStatus{Component: c, Own: own, Status: st})
		if st == objects.UpdateRequired {
			ev.Required = true
		}
	}
	return ev, len(ev.Components) > 0
}

// Latest returns the raw object of the last applied manifest, or nil. pex/2 hands it out.
func (m *Manager) Latest() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.latest == nil {
		return nil
	}
	return append([]byte(nil), m.latest...)
}

// Last returns the last applied manifest, or nil.
func (m *Manager) Last() *objects.Manifest { return m.state.Load().Last() }

// Keys returns the current release key set.
func (m *Manager) Keys() []keys.AKey { return m.state.Load().Keys() }

// Policy returns the effective policy of a component, or nil.
func (m *Manager) Policy(component string) *pb.Policy { return m.state.Load().Policy(component) }

// PeerRefused implements session.PeerPolicy. Never lock out: only peers strictly below the node's
// own version are refused (objects.ReleaseState).
func (m *Manager) PeerRefused(component string, own, peer uint32, now time.Time) bool {
	return m.state.Load().PeerRefused(component, own, peer, now)
}

// Admit returns the handshake version admission for a component and the node's own version
// (session.AcceptConfig.AdmitVersion): false makes the responder answer REJECT{version}.
func (m *Manager) Admit(component string, own uint32) func(peerVersion uint32) bool {
	return func(peer uint32) bool {
		return !m.state.Load().PeerRefused(component, own, peer, m.cfg.Clock.Now())
	}
}

// Status returns the update status of the node's own version of a component (Config.Own).
func (m *Manager) Status(component string) objects.UpdateStatus {
	own, ok := m.cfg.Own[component]
	if !ok {
		return objects.UpToDate
	}
	return m.state.Load().Status(component, own, m.cfg.Clock.Now())
}

// DHTSource is the lookup client (dht.Client).
type DHTSource interface {
	GetRelease(ctx context.Context, valid func(raw []byte, now time.Time) error) (*objects.Envelope, error)
}

// PollDHT looks the manifest of the day up in the DHT and offers it. Only records that pass the
// manager's check take part in the DHT client's choice.
func (m *Manager) PollDHT(ctx context.Context, src DHTSource) (bool, error) {
	env, err := src.GetRelease(ctx, m.Check)
	if err != nil {
		return false, err
	}
	if env == nil {
		return false, errors.New("release: empty DHT answer")
	}
	return m.Offer(env.Raw)
}

// FetchSite downloads the manifest from the download page and offers it. The URL and every redirect
// must be https; the body is bounded by MaxManifestSize before it is read into memory.
func (m *Manager) FetchSite(ctx context.Context, hc *http.Client, rawURL string) (bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false, fmt.Errorf("%w: https URL required", ErrSource)
	}
	if hc == nil {
		return false, fmt.Errorf("%w: no HTTP client", ErrSource)
	}
	c := *hc
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("%w: redirect to %s", ErrSource, req.URL.Scheme)
		}
		if len(via) > maxRedirects {
			return fmt.Errorf("%w: too many redirects", ErrSource)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrSource, err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%w: status %d", ErrSource, resp.StatusCode)
	}
	if resp.ContentLength > MaxManifestSize {
		return false, ErrTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxManifestSize+1))
	if err != nil {
		return false, err
	}
	if len(body) > MaxManifestSize {
		return false, ErrTooLarge
	}
	return m.Offer(body)
}

// Err reports why the stored chain is unavailable (an ErrChainUnavailable), or nil.
func (m *Manager) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.broken
}

// Reset forgets the stored chain and starts again from the compiled keys. It is the explicit
// repair after a lost chain (or a build with other release keys): the key set it returns to may
// contain keys the lost chain had revoked, so the caller decides, not the manager.
func (m *Manager) Reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if kv := m.cfg.KV; kv != nil {
		for i := 0; i < 8; i++ {
			rest, err := kv.Scan(chainPref, store.MaxScanLimit)
			if err != nil {
				return fmt.Errorf("release: reset: %w", err)
			}
			if len(rest) == 0 {
				break
			}
			for _, e := range rest {
				if err := kv.Delete(append(append([]byte(nil), chainPref...), e.Key[len(chainPref):]...)); err != nil {
					return fmt.Errorf("release: reset: %w", err)
				}
			}
		}
		if err := kv.Delete(versionKey); err != nil {
			return fmt.Errorf("release: reset: %w", err)
		}
	}
	st, err := objects.NewReleaseState(m.cfg.Keys)
	if err != nil {
		return err
	}
	m.state.Store(st)
	m.latest, m.chainLen, m.broken = nil, 0, nil
	return nil
}
