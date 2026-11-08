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
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/convsync"
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
	// ErrTooManyContacts reports a new contact beyond the limit.
	ErrTooManyContacts = errors.New("app: too many contacts")
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

// Chat is the part of the chat service the contact knock uses.
type Chat interface {
	Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) ([16]byte, error)
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
	// MaxContacts bounds the contact list; zero means 10000. A link beyond it is refused before
	// anything is dialed.
	MaxContacts int
	// Chat sends knocks; nil disables Knock and the answers to knocks.
	Chat Chat
	// OwnCard returns the node's current peer card envelope, the answer to a knock.
	OwnCard func() ([]byte, bool)
	// IngestCard receives the card a contact answered a knock with. The receiver verifies it
	// (objects.VerifyCard, the account must be the sender); the app only passes bounded bytes
	// from contacts.
	IngestCard func(from [32]byte, envelope []byte)
	// Convs is the conversation engine behind the chats; nil turns the chat calls off.
	Convs Convs
	// PubOf finds the account key behind a node ID (the directory); chats need it to open a
	// direct conversation.
	PubOf func(ctx context.Context, node [32]byte) ([32]byte, error)
	// Groups is the register of the groups this node is in; the conversation engine is built with its
	// rules (Rules, Conv, Members, SharedGroups). Nil turns the group calls off.
	Groups *GroupBook
	// Voice is the live roster of the voice rooms; nil turns GroupVoice off.
	Voice VoiceRooms
	// States tells the delivery state of the own messages; nil leaves them without one.
	States StateSource
	// Self is this node's account node ID, to refuse a link to oneself.
	Self [32]byte
	// Files holds the files this node sends (*files.Server); nil turns sending off.
	Files FileHolder
	// FileNet fetches a file from its sender (FetchOver builds the usual one); nil turns receiving off.
	FileNet FileNet
	// FileDir is where received files go. FileLimits bound sizes and numbers; FileFree reports free
	// disk space (nil: the system's); FileWait waits between retries of a fetch (nil: a timer).
	FileDir    string
	FileLimits FileLimits
	FileFree   func(dir string) (uint64, error)
	FileWait   func(ctx context.Context, d time.Duration) bool
	// Prefs holds the notification settings and followed threads of this device; nil turns them off.
	Prefs *convsync.Prefs
	// VerifyCard checks the card a contact answered a knock with: true when it is a valid card of
	// that account. Nil: the real check (objects.VerifyCard, the card's account is the sender).
	VerifyCard func(from [32]byte, envelope []byte) bool
	// Exit dials the exit a link names (the runtime's ExitClient behind a card); nil turns the VPN off.
	// VPNListen is the address of the local proxy, "127.0.0.1:1080" when empty.
	Exit      ExitDialer
	VPNListen string
	// CallCooldown is the least time between two new calls announced by one member of a group; zero means
	// 30 s, negative turns it off. MaxGroupCalls bounds the calls going on at once in one group (its own
	// and its voice rooms'); zero means 16. A call beyond either is not shown: silence.
	CallCooldown  time.Duration
	MaxGroupCalls int
	// JoinHost is the host of the join page for the https form of group invitations ("": none).
	JoinHost string
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
	kmu     sync.Mutex // knock answers: last answer per contact
	knocked map[[32]byte]time.Time
	asked   map[[32]byte]time.Time // knocks sent and not yet answered, by contact
	ntc     notices                // notices for the UI (notice.go)
	chats   chatState
	hmu     sync.Mutex // history handed to newcomers: when, per group and key
	handed  map[string]time.Time
	fx      fileState
	gev     groupEvents
	vpn     vpnState
	gc      callBook
	vmu     sync.Mutex // the saved exits and the history
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
	if err := checkLoopback(cfg.VPNListen); err != nil {
		return nil, err
	}
	a := &App{cfg: cfg}
	if cfg.Groups != nil && cfg.Convs != nil {
		cfg.Groups.attach(cfg.Convs.Log())
		cfg.Convs.Log().OnChange(a.onGroupChange)
	}
	return a, nil
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
	a.stopFiles()
	return a.cfg.Runtime.Stop(ctx)
}

// Running reports whether the app started the runtime.
func (a *App) Running() bool { return a.running.Load() }
