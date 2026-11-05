package node

import (
	"context"

	"github.com/echofog/echofog-core/internal/v2/mailbox"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// MailboxNamespace is the store namespace of mailbox/2 queues (AD13: persistent state lives in
// the store).
const MailboxNamespace = "v2/mailbox"

// registerMailbox registers mailbox/2 (T7.3) when the platform profile carries the mailbox role
// (platforms.md §2). Without the role the service is not offered, so peers get UNSUPPORTED.
//
// The relay stores deposits only for others; on a metered profile it still answers drains of
// existing queues but refuses new queues and deposits (mailbox.Config.Metered).
func (r *Runtime) registerMailbox() {
	if !profileHasRole(r.cfg.Profile.Roles, RoleMailbox) {
		return
	}
	spec, ok := service.Builtin(service.Mailbox)
	if !ok {
		return
	}
	srv, err := mailbox.New(mailbox.Config{
		Self:    [32]byte(r.identity.NodeID),
		HS:      r.cfg.Device.Handshake(),
		Store:   r.cfg.Store.Store(MailboxNamespace),
		Now:     r.clock.Now,
		Log:     r.log,
		Metered: r.cfg.Profile.Metered,
	})
	if err != nil {
		r.log.Warn("node: mailbox relay", "err", err)
		return
	}
	r.Register(spec, func(ctx context.Context, p Peer, st *mux.Stream) {
		srv.Handle(ctx, mailbox.Peer{
			Node:   [32]byte(p.Node),
			Device: [32]byte(p.Device),
			SID:    p.SID,
			Path:   p.Path,
			Remote: p.Remote,
		}, st)
	})
}

func profileHasRole(roles []Role, want Role) bool {
	for _, role := range roles {
		if role == want {
			return true
		}
	}
	return false
}
