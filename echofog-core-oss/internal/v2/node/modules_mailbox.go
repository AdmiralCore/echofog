package node

import (
	"context"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mailbox"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// MailboxNamespace is the store namespace of mailbox/2 queues (AD13: persistent state lives in
// the store).
const MailboxNamespace = "v2/mailbox"

// registerMailbox registers mailbox/2 (T7.3, T7.4). A node with the mailbox role (platforms.md §2) is
// a mailbox relay M and a forwarding relay F. Every other node offers the service as a client only
// (T7.4): its handler refuses any stream a peer opens, so a node that is no relay stores and
// forwards nothing, while it can still open mailbox/2 streams to relays.
//
// The relay stores deposits only for others; on a metered profile it still answers drains of
// existing queues but refuses new queues and deposits (mailbox.Config.Metered).
func (r *Runtime) registerMailbox() {
	spec, ok := service.Builtin(service.Mailbox)
	if !ok {
		return
	}
	if !profileHasRole(r.cfg.Profile.Roles, RoleMailbox) {
		r.Register(spec, func(ctx context.Context, p Peer, st *mux.Stream) {
			st.Reset(frame.CodeServiceUnsupported)
		})
		return
	}
	srv, err := mailbox.New(mailbox.Config{
		Self:       [32]byte(r.identity.NodeID),
		SelfDevice: [32]byte(r.identity.DeviceID),
		HS:         r.cfg.Device.Handshake(),
		Store:      r.cfg.Store.Store(MailboxNamespace),
		Now:        r.clock.Now,
		Log:        r.log,
		Metered:    r.cfg.Profile.Metered,
		Forwarder:  mailboxForwarder{r: r},
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

// MailboxDialer opens mailbox/2 sessions for a client (a recipient fetching its queues, a sender
// handing a deposit to F). opts chooses the dial options for a device; nil means the card book.
func (r *Runtime) MailboxDialer(opts func(device [32]byte) DialOpt) mailbox.Dialer {
	return mailboxDialer{r: r, opts: opts}
}

type mailboxDialer struct {
	r    *Runtime
	opts func(device [32]byte) DialOpt
}

func (d mailboxDialer) Dial(ctx context.Context, device [32]byte) (mailbox.Conn, error) {
	var opt DialOpt
	if d.opts != nil {
		opt = d.opts(device)
	}
	p, st, err := d.r.Open(ctx, DeviceID(device), service.Mailbox, opt)
	if err != nil {
		return nil, err
	}
	return mailbox.NewStreamConn(st, p.SID, nil), nil
}
