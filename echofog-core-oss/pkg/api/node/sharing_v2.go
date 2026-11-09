package node

import (
	"context"
	"time"

	"github.com/echofog/echofog-core/internal/v2/app"
)

// The exit of a desktop at protocol 2 (T10.9) on the facade: the sharing card of the interface turns
// it on and off, and the owner confirms the clients that tried. It is free: a price is refused.

// v2Exit is the exit of *app.App. A separate interface: an app built without a host for the exit
// answers ExitSupported() false and the facade then behaves as before.
type v2Exit interface {
	ExitSupported() bool
	ExitEnable(ctx context.Context, s app.ExitSettings) (app.ExitInfo, error)
	ExitDisable() error
	ExitStatus() app.ExitStatusView
	ExitApprove(id, label string) error
	ExitReject(id string) error
	ExitRevoke(id string) error
	ExitAllowAny(ctx context.Context, ttl time.Duration) (app.ExitInfo, error)
	ExitRevokeAny() error
	ExitLink(ctx context.Context) (app.ExitInfo, error)
}

var _ v2Exit = (*app.App)(nil)

// exitWait covers the longest call of the exit to the card builder and the links.
const exitWait = 30 * time.Second

// maxOpenMinutes bounds the open access: a week.
const maxOpenMinutes = 60 * 24 * 7

// exit is the exit of the app when it has one.
func (b *v2VPNBackend) exit() (v2Exit, bool) {
	e, ok := b.app.(v2Exit)
	if !ok || !e.ExitSupported() {
		return nil, false
	}
	return e, true
}

// sharingStart turns the exit on.
func (b *v2VPNBackend) sharingStart(cfg SharingConfig) error {
	e, ok := b.exit()
	if !ok {
		return ErrV2NotBuilt
	}
	if cfg.PricePerMB > 0 {
		return &v2UserError{msg: "В этой версии выход бесплатный: цену пока задать нельзя"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), exitWait)
	defer cancel()
	_, err := e.ExitEnable(ctx, app.ExitSettings{MaxMbps: cfg.UploadKbps / 1000})
	return v2Err(err)
}

func (b *v2VPNBackend) sharingStop() error {
	e, ok := b.exit()
	if !ok {
		return ErrV2NotBuilt
	}
	return v2Err(e.ExitDisable())
}

func (b *v2VPNBackend) sharingStatus() SharingStatus {
	e, ok := b.exit()
	if !ok {
		return SharingStatus{}
	}
	v := e.ExitStatus()
	st := SharingStatus{Enabled: v.Enabled, Accepted: v.Accepted, Refused: v.Refused, RefusedBy: v.RefusedBy, Bytes: v.Bytes,
		Config: SharingConfig{UploadKbps: v.MaxMbps * 1000}}
	acc := &SharingAccess{AnyUntil: v.AnyUntil, Clients: []ExitClientDTO{}, Waiting: []ExitWaitDTO{}, Confirmed: []ExitKnownDTO{}}
	if v.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), exitWait)
		if info, err := e.ExitLink(ctx); err == nil {
			acc.Link, acc.Web = info.Link, info.Web
		}
		cancel()
	}
	for _, c := range v.Clients {
		acc.Clients = append(acc.Clients, ExitClientDTO{ID: c.ID, Label: c.Label, ByLink: c.ByLink, Streams: c.Streams})
	}
	for _, p := range v.Pending {
		acc.Waiting = append(acc.Waiting, ExitWaitDTO{ID: p.ID, Since: p.Since, Last: p.Last, Tries: p.Tries})
	}
	for _, p := range v.Approved {
		acc.Confirmed = append(acc.Confirmed, ExitKnownDTO{ID: p.ID, Label: p.Label, Since: p.Since})
	}
	st.Access = acc
	return st
}

// SharingApprove confirms a client waiting for the owner and remembers it under label.
func (n *NodeApi) SharingApprove(id, label string) error {
	e, ok := n.sharingExit()
	if !ok {
		return ErrV2NotBuilt
	}
	return v2Err(e.ExitApprove(id, label))
}

// SharingReject turns a waiting client down.
func (n *NodeApi) SharingReject(id string) error {
	e, ok := n.sharingExit()
	if !ok {
		return ErrV2NotBuilt
	}
	return v2Err(e.ExitReject(id))
}

// SharingRevoke forgets a confirmed client and closes its streams.
func (n *NodeApi) SharingRevoke(id string) error {
	e, ok := n.sharingExit()
	if !ok {
		return ErrV2NotBuilt
	}
	return v2Err(e.ExitRevoke(id))
}

// SharingOpenFor lets anyone with the link use the exit for the given minutes (at most a week).
func (n *NodeApi) SharingOpenFor(minutes int) error {
	e, ok := n.sharingExit()
	if !ok {
		return ErrV2NotBuilt
	}
	if minutes <= 0 || minutes > maxOpenMinutes {
		return v2Err(app.ErrExitBadTTL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), exitWait)
	defer cancel()
	_, err := e.ExitAllowAny(ctx, time.Duration(minutes)*time.Minute)
	return v2Err(err)
}

// SharingCloseOpen ends the open access.
func (n *NodeApi) SharingCloseOpen() error {
	e, ok := n.sharingExit()
	if !ok {
		return ErrV2NotBuilt
	}
	return v2Err(e.ExitRevokeAny())
}

func (n *NodeApi) sharingExit() (v2Exit, bool) {
	b := n.v2vpn()
	if b == nil {
		return nil, false
	}
	return b.exit()
}
