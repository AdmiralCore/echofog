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

	errNotImplemented = errors.New("app: not implemented")
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
type App struct{ cfg Config }

// New creates an App.
func New(cfg Config) (*App, error) { return &App{cfg: cfg}, nil }

// Start starts the runtime.
func (a *App) Start(ctx context.Context) error { return errNotImplemented }

// Stop stops the runtime.
func (a *App) Stop(ctx context.Context) error { return errNotImplemented }

// Running reports whether the app started the runtime.
func (a *App) Running() bool { return false }
