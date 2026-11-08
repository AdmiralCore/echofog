package app

import (
	"context"
	"encoding/hex"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node"
)

// The exit of this node (T10.9): a desktop that offers its connection to clients the owner has
// confirmed, or to anyone who has the link for a limited time. It is off after every start.
//
// A client reaches the exit under the identity it uses for this exit alone (services.md section 7);
// the exit never sees the messenger identity. So the owner confirms identities: the first attempt of
// an unknown one is refused with a policy refusal (the same as an exit that is off) and waits in a
// bounded queue; once confirmed it is served and remembered. exit/2 has no field for a secret, so a
// personal link with a secret is not possible without a change of the wire.

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

type exitState struct {
	mu      sync.Mutex
	access  *ExitAccess
	enabled bool
	maxMbps int
	gen     int // bumped by every change of the open window, to stop an old timer
}

func (a *App) exitAccess() *ExitAccess {
	e := &a.exit
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.access == nil {
		e.access = NewExitAccess(a.cfg.Clock, a.cfg.Store)
	}
	return e.access
}

// ExitEnable turns the exit on and returns its link. It is off after every start; nothing is kept of
// this call but the confirmed clients.
func (a *App) ExitEnable(ctx context.Context, s ExitSettings) (ExitInfo, error) {
	if a.cfg.ExitHost == nil {
		return ExitInfo{}, ErrExitOff
	}
	x := a.exitAccess()
	err := a.cfg.ExitHost.EnableExit(node.ExitOptions{
		Admit: func(n [32]byte, _ netip.AddrPort) bool { return x.Admit(n) },
		Refused: func(n [32]byte, remote netip.AddrPort) {
			src := "unknown"
			if remote.IsValid() {
				src = remote.Addr().String()
			}
			x.Refused(n, src)
		},
		MaxMbps:             s.MaxMbps,
		AllowPrivateTargets: s.AllowPrivateTargets,
	})
	if err != nil {
		return ExitInfo{}, err
	}
	a.exit.mu.Lock()
	a.exit.enabled, a.exit.maxMbps = true, s.MaxMbps
	a.exit.mu.Unlock()
	return a.ExitLink(ctx)
}

// ExitDisable turns the exit off: new streams are refused and the live ones closed.
func (a *App) ExitDisable() error {
	if a.cfg.ExitHost == nil {
		return ErrExitOff
	}
	x := a.exitAccess()
	x.RevokeAny()
	a.exit.mu.Lock()
	a.exit.enabled = false
	a.exit.gen++
	a.exit.mu.Unlock()
	a.cfg.ExitHost.DisableExit()
	return nil
}

func (a *App) exitOn() bool {
	a.exit.mu.Lock()
	defer a.exit.mu.Unlock()
	return a.exit.enabled
}

// ExitStatus reports the exit.
func (a *App) ExitStatus() ExitStatusView {
	if a.cfg.ExitHost == nil {
		return ExitStatusView{}
	}
	a.exit.mu.Lock()
	enabled, mbps := a.exit.enabled, a.exit.maxMbps
	a.exit.mu.Unlock()
	x := a.exitAccess()
	v := ExitStatusView{Enabled: enabled, Pending: x.Pending(), Approved: x.Approved(), AnyUntil: x.AnyUntil(), MaxMbps: mbps}
	if !enabled {
		return v
	}
	hs := a.cfg.ExitHost.ExitStatus()
	v.Accepted, v.Refused, v.Bytes, v.RefusedBy = hs.Accepted, hs.Refused, hs.Bytes, hs.RefusedBy
	labels := map[string]string{}
	for _, ap := range v.Approved {
		labels[ap.ID] = ap.Label
	}
	for _, c := range hs.Clients {
		id := hex.EncodeToString(c.Node[:])
		label, known := labels[id]
		v.Clients = append(v.Clients, ExitClientView{ID: id, Label: label, ByLink: !known, Streams: c.Streams})
	}
	return v
}

// ExitApprove confirms a client waiting for approval and remembers it under label.
func (a *App) ExitApprove(id, label string) error {
	if a.cfg.ExitHost == nil {
		return ErrExitOff
	}
	return a.exitAccess().Approve(id, label)
}

// ExitReject turns a waiting client down.
func (a *App) ExitReject(id string) error {
	if a.cfg.ExitHost == nil {
		return ErrExitOff
	}
	return a.exitAccess().Reject(id)
}

// ExitRevoke forgets a confirmed client and closes its streams.
func (a *App) ExitRevoke(id string) error {
	if a.cfg.ExitHost == nil {
		return ErrExitOff
	}
	if err := a.exitAccess().Revoke(id); err != nil {
		return err
	}
	a.cfg.ExitHost.RecheckExit()
	return nil
}

// ExitAllowAny lets anyone with the link use the exit for ttl; the owner asks for it explicitly. The
// access ends by itself: the streams it admitted are closed when it does.
func (a *App) ExitAllowAny(ctx context.Context, ttl time.Duration) (ExitInfo, error) {
	if a.cfg.ExitHost == nil {
		return ExitInfo{}, ErrExitOff
	}
	if !a.exitOn() {
		return ExitInfo{}, ErrExitNotOn
	}
	x := a.exitAccess()
	secs := int64((ttl + time.Second - 1) / time.Second)
	until := x.AllowAny(secs)
	if until == 0 {
		return ExitInfo{}, ErrExitBadTTL
	}
	a.exit.mu.Lock()
	a.exit.gen++
	gen := a.exit.gen
	a.exit.mu.Unlock()
	go a.exitWatchWindow(gen, ttl)
	info, err := a.ExitLink(ctx)
	info.Until = until
	return info, err
}

// ErrExitBadTTL reports an open access without a positive time.
var ErrExitBadTTL = errors.New("app: the open access needs a time")

// exitWatchWindow closes the streams the open access admitted once it is over.
func (a *App) exitWatchWindow(gen int, ttl time.Duration) {
	t := time.NewTimer(ttl)
	defer t.Stop()
	for {
		<-t.C
		a.exit.mu.Lock()
		current := a.exit.gen == gen && a.exit.enabled
		a.exit.mu.Unlock()
		if !current {
			return
		}
		if a.exitAccess().AnyUntil() == 0 {
			a.cfg.ExitHost.RecheckExit()
			return
		}
		t.Reset(200 * time.Millisecond) // the clock counts whole seconds: look again
	}
}

// ExitRevokeAny ends the open access and closes the streams of those it admitted.
func (a *App) ExitRevokeAny() error {
	if a.cfg.ExitHost == nil {
		return ErrExitOff
	}
	a.exitAccess().RevokeAny()
	a.exit.mu.Lock()
	a.exit.gen++
	a.exit.mu.Unlock()
	a.cfg.ExitHost.RecheckExit()
	return nil
}

// ExitLink is the link to the running exit: the contact link, whose card carries the exit offer.
func (a *App) ExitLink(ctx context.Context) (ExitInfo, error) {
	if a.cfg.ExitHost == nil {
		return ExitInfo{}, ErrExitOff
	}
	if !a.exitOn() {
		return ExitInfo{}, ErrExitNotOn
	}
	l, err := a.cfg.Links.ContactLink(ctx)
	if err != nil {
		return ExitInfo{}, err
	}
	return ExitInfo{Link: l.URL, Web: l.Web, Until: a.exitAccess().AnyUntil()}, nil
}
