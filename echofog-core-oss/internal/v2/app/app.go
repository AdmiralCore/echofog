// Package app is the application layer of a v2 desktop node (T11.2): start and stop of the
// runtime, the access mode (public or hidden), links (contact, invitation with an access term,
// opening, preview), passes, contacts, the network status and the connection report.
//
// It does not import pkg/node (AD6/AD8); pkg/api/node turns its types into the DTOs of the
// frontend. Everything the package needs from the runtime sits behind two small interfaces, so the
// package is tested without a desktop and without a network.
package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/routepolicy"
	"github.com/echofog/echofog-core/internal/v2/store"
)

var (
	// ErrRunning reports Start on a running app.
	ErrRunning = errors.New("app: already running")
	// ErrMode reports a mode other than public or hidden.
	ErrMode = errors.New("app: unknown access mode")
	// ErrNotHidden reports an invitation request on a public node.
	ErrNotHidden = errors.New("app: invitations need a hidden node")
	// ErrSelf reports a link to this very node.
	ErrSelf = errors.New("app: link to this node")
	// ErrBlocked reports a link to a blocked contact.
	ErrBlocked = errors.New("app: contact is blocked")
	// ErrAlias reports an alias that is empty, too long or has control characters.
	ErrAlias = errors.New("app: bad alias")
	// ErrNoContact reports an unknown contact.
	ErrNoContact = errors.New("app: no such contact")
	// ErrNoPass reports an unknown pass.
	ErrNoPass = errors.New("app: no such pass")
	// ErrRequest reports a request with a bad term or count.
	ErrRequest = errors.New("app: bad request")
)

// Mode is the access mode of the node (the desktop offers these two).
type Mode string

const (
	// ModePublic: the node has a public card and a standing contact link.
	ModePublic Mode = "public"
	// ModeHidden: the node has no public card; people come in by invitations.
	ModeHidden Mode = "hidden"
)

// Runtime is the part of *node.Runtime the app uses.
type Runtime interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status() node.Status
	NetClass() routepolicy.NetClass
	RouteReport(peer node.DeviceID) (node.RouteReport, bool)
	AccessList(ctx context.Context) ([]access.Info, error)
	RevokeAccess(ctx context.Context, id access.PassID) (*access.RevokeResult, error)
	ExtendAccess(ctx context.Context, id access.PassID, d time.Duration) (access.Info, error)
	RotateAccessKeys(ctx context.Context) (*access.Rotation, error)
}

// Links is the part of *links.Service the app uses.
type Links interface {
	ContactLink(ctx context.Context) (*links.Link, error)
	Invite(ctx context.Context, opt access.MintOptions) (*links.Invitation, error)
	Open(link string) (*links.Preview, error)
	Redeem(ctx context.Context, link string) (node.Peer, error)
}

// Clock is the node's corrected clock.
type Clock interface{ Now() time.Time }

// Config configures an App.
type Config struct {
	Runtime Runtime
	Links   Links
	// Store keeps the app's own records (contacts, the saved mode). Required.
	Store store.KV
	Clock Clock
	// Self is this node's account node ID, to refuse a link to oneself.
	Self [32]byte
	// Mode is the mode the node was built in; Version is the app version shown in the report.
	Mode    Mode
	Version string
}

// App is the application layer of one node. It is safe for concurrent use.
type App struct {
	cfg Config

	lc      sync.Mutex // serializes Start and Stop
	running atomic.Bool
	cmu     sync.Mutex // contacts: read-modify-write
	ntc     notices    // notices for the UI (notice.go)
}

// New creates an App.
func New(cfg Config) (*App, error) {
	switch {
	case cfg.Runtime == nil:
		return nil, errors.New("app: a runtime is required")
	case cfg.Links == nil:
		return nil, errors.New("app: a link service is required")
	case cfg.Store == nil:
		return nil, errors.New("app: a store is required")
	case cfg.Clock == nil:
		return nil, errors.New("app: a clock is required")
	}
	if cfg.Mode == "" {
		cfg.Mode = ModePublic
	}
	if !cfg.Mode.valid() {
		return nil, fmt.Errorf("%w: %q", ErrMode, cfg.Mode)
	}
	return &App{cfg: cfg}, nil
}

func (m Mode) valid() bool { return m == ModePublic || m == ModeHidden }

// Start starts the runtime. A failed start leaves the app stopped.
func (a *App) Start(ctx context.Context) error {
	a.lc.Lock()
	defer a.lc.Unlock()
	if a.running.Load() {
		return ErrRunning
	}
	if err := a.cfg.Runtime.Start(ctx); err != nil {
		return err
	}
	a.running.Store(true)
	return nil
}

// Stop stops the runtime. Stopping a stopped app does nothing.
func (a *App) Stop(ctx context.Context) error {
	a.lc.Lock()
	defer a.lc.Unlock()
	if !a.running.Load() {
		return nil
	}
	a.running.Store(false)
	return a.cfg.Runtime.Stop(ctx)
}

// Running reports whether the app started the runtime.
func (a *App) Running() bool { return a.running.Load() }
