// Package release keeps the release manifest of a node (objects.md §9, versioning.md §5): the key
// set and the last applied manifest persisted in the store, the sources that offer manifests (DHT,
// pex/2, the download page), the version policy of the handshake, and the "update available"
// events. The checks themselves live in objects.ReleaseState; this package never repeats them.
package release

import (
	"context"
	"errors"
	"net/http"
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
)

// Errors.
var (
	ErrNotImplemented = errors.New("release: not implemented")
	ErrTooLarge       = errors.New("release: manifest too large")
	ErrStale          = errors.New("release: not newer than the applied manifest")
	ErrChainFull      = errors.New("release: manifest chain is full")
	ErrSource         = errors.New("release: bad source")
)

// Clock is the node's corrected clock (AD13).
type Clock interface{ Now() time.Time }

// Event reports an applied manifest that says the node's own version is outdated.
type Event struct {
	Version  string
	NotesURL string
	At       time.Time
	// Components lists the components of the node whose status is not up to date.
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
	Keys    []keys.AKey
	KV      store.KV
	Clock   Clock
	Own     map[string]uint32
	OnEvent func(Event)
}

// Manager is the release state of one node.
type Manager struct{}

// Open creates a Manager and replays the persisted chain.
func Open(Config) (*Manager, error) { return &Manager{}, nil }

// Check verifies a release object without applying it (DHT ReleaseCheck).
func (m *Manager) Check([]byte, time.Time) error { return ErrNotImplemented }

// Offer applies a release object from any source.
func (m *Manager) Offer([]byte) (bool, error) { return false, ErrNotImplemented }

// Latest returns the raw object of the last applied manifest, or nil.
func (m *Manager) Latest() []byte { return nil }

// Last returns the last applied manifest, or nil.
func (m *Manager) Last() *objects.Manifest { return nil }

// Keys returns the current release key set.
func (m *Manager) Keys() []keys.AKey { return nil }

// Policy returns the effective policy of a component.
func (m *Manager) Policy(string) *pb.Policy { return nil }

// PeerRefused implements session.PeerPolicy.
func (m *Manager) PeerRefused(string, uint32, uint32, time.Time) bool { return false }

// Admit returns the handshake version admission for a component and the node's own version.
func (m *Manager) Admit(string, uint32) func(uint32) bool { return nil }

// Status returns the update status of the node's own version of a component.
func (m *Manager) Status(string) objects.UpdateStatus { return objects.UpToDate }

// DHTSource is the lookup client (dht.Client).
type DHTSource interface {
	GetRelease(ctx context.Context, valid func(raw []byte, now time.Time) error) (*objects.Envelope, error)
}

// PollDHT looks the manifest up in the DHT and offers it.
func (m *Manager) PollDHT(context.Context, DHTSource) (bool, error) { return false, ErrNotImplemented }

// FetchSite downloads the manifest from the download page (https only) and offers it.
func (m *Manager) FetchSite(context.Context, *http.Client, string) (bool, error) {
	return false, ErrNotImplemented
}
