package app

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node"
)

// The exit of this node (T10.9): a desktop that offers its connection to clients the owner has
// confirmed, or to anyone who has the link for a limited time. It is off after every start.

// ExitHost is the part of *node.Runtime that runs the exit.
type ExitHost interface {
	EnableExit(opt node.ExitOptions) error
	DisableExit()
	RecheckExit()
	ExitStatus() node.ExitStatus
}

// Exit errors.
var (
	// ErrExitOff reports an exit call on an app built without a host for it.
	ErrExitOff = errors.New("app: the exit is not available")
	// ErrExitNotOn reports a call that needs the exit running.
	ErrExitNotOn = errors.New("app: the exit is off")
)

// ExitSettings are the choices of the owner.
type ExitSettings struct {
	// MaxMbps limits one client, in megabits a second; 0 keeps the default.
	MaxMbps int
	// AllowPrivateTargets is for tests and closed deployments.
	AllowPrivateTargets bool
}

// ExitInfo is how others reach the exit.
type ExitInfo struct {
	// Link is the link that VPNStart takes: the exit's contact link.
	Link string
	// Web is the https form of the link, when there is one.
	Web string
	// Until is when access for anyone with the link ends (unix seconds); 0 when it is not open.
	Until int64
}

// ExitClientView is a client connected now.
type ExitClientView struct {
	ID      string
	Label   string // what the owner called it; empty for someone who came by the open link
	ByLink  bool
	Streams int
}

// ExitStatusView is the state of the exit.
type ExitStatusView struct {
	Enabled           bool
	Accepted, Refused uint64
	Bytes             uint64
	RefusedBy         map[string]uint64
	Clients           []ExitClientView
	Pending           []ExitPendingView
	Approved          []ExitApprovedView
	AnyUntil          int64
	MaxMbps           int
}

// ExitEnable turns the exit on and returns its link.
func (a *App) ExitEnable(ctx context.Context, s ExitSettings) (ExitInfo, error) {
	return ExitInfo{}, errors.New("not implemented")
}

// ExitDisable turns the exit off: new streams are refused and the live ones closed.
func (a *App) ExitDisable() error { return errors.New("not implemented") }

// ExitStatus reports the exit.
func (a *App) ExitStatus() ExitStatusView { return ExitStatusView{} }

// ExitApprove confirms a client waiting for approval and remembers it under label.
func (a *App) ExitApprove(id, label string) error { return errors.New("not implemented") }

// ExitReject turns a waiting client down.
func (a *App) ExitReject(id string) error { return errors.New("not implemented") }

// ExitRevoke forgets a confirmed client and closes its streams.
func (a *App) ExitRevoke(id string) error { return errors.New("not implemented") }

// ExitAllowAny lets anyone with the link use the exit for ttl; the owner asks for it explicitly.
func (a *App) ExitAllowAny(ctx context.Context, ttl time.Duration) (ExitInfo, error) {
	return ExitInfo{}, errors.New("not implemented")
}

// ExitRevokeAny ends the open access and closes the streams of those it admitted.
func (a *App) ExitRevokeAny() error { return errors.New("not implemented") }

// ExitLink is the link to the running exit.
func (a *App) ExitLink(ctx context.Context) (ExitInfo, error) {
	return ExitInfo{}, errors.New("not implemented")
}
