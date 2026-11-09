package node

import (
	"context"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/services/contacts"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/pkg/dto"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// v2App is the part of *app.App the protocol 2 network and contacts backends use (T11.2).
type v2App interface {
	AccessStatus(ctx context.Context) (app.AccessStatus, error)
	SetMode(m app.Mode) error
	Invite(ctx context.Context, r app.InviteRequest) (app.Invitation, error)
	Passes(ctx context.Context) ([]app.PassView, error)
	ExtendPass(ctx context.Context, id string, days float64) (app.PassView, error)
	RevokePass(ctx context.Context, id string) (app.PassView, error)
	RotateAccessKeys(ctx context.Context) error
	Preview(text string) (app.LinkInfo, error)
	AddByLink(ctx context.Context, text string) (app.Contact, error)
	Contacts() ([]app.Contact, error)
	SetAlias(node, alias string) error
	DeleteContact(node string) error
	Block(node string) error
	Unblock(node string) error
	NetStatus() app.NetStatus
	Report() string
	Subscribe(fn func(app.Notice)) (cancel func())
	LastUpdate() *app.UpdateView
}

var _ v2App = (*app.App)(nil)

// v2OpTimeout bounds a UI call that dials: joining by a link.
const v2OpTimeout = 60 * time.Second

// v2UserError turns an error of the app layer into one the UI can show as it is: a Russian text in
// a neutral form that never repeats an internal error.
type v2UserError struct {
	msg    string
	screen *app.ErrorScreen // the full-screen error this error has, if any
}

func (e *v2UserError) Error() string { return e.msg }

func v2Err(err error) error {
	if err == nil {
		return nil
	}
	e := &v2UserError{msg: app.UserMessage(err)}
	if sc, ok := app.Screen(err); ok {
		e.screen = &sc
	}
	return e
}

// v2NetBackend is the protocol 2 network and access backend. The methods the app layer does not
// have yet answer ErrV2NotBuilt through the embedded pending backend.
type v2NetBackend struct {
	v2PendingBackend
	app v2App
	api *NodeApi // the saved settings (proxy, anonymous mode) live on the facade
	// check says whether the runtime can run in a mode; restart rebuilds it from the saved settings
	// (T11.7). Both nil: the mode applies at the next start.
	check   func(app.Mode) error
	restart func() error
}

// v2ContactBackend is the protocol 2 contacts backend.
type v2ContactBackend struct {
	v2PendingBackend
	app v2App
}

// AttachV2 gives the facade the v2 app layer: the network, access and contacts domains answer from
// it from now on. Call it once, before the UI reads the facade. At protocol 1 it does nothing.
func (n *NodeApi) AttachV2(a v2App) {
	if a == nil || n.node == nil || n.node.Cfg == nil || !n.node.Cfg.IsV2() {
		return
	}
	n.backends.net = &v2NetBackend{app: a, api: n, check: n.v2CheckMode, restart: n.restartV2}
	n.backends.contact = &v2ContactBackend{app: a}
	n.v2n.attach(a)
}

func v2Ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), v2OpTimeout)
}

func (b *v2NetBackend) GetAccessStatus() (AccessStatus, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	st, err := b.app.AccessStatus(ctx)
	if err != nil {
		return AccessStatus{}, v2Err(err)
	}
	out := AccessStatus{Mode: string(st.Mode), NextMode: string(st.NextMode), Link: st.Link, WebLink: st.WebLink,
		LinkError: st.LinkError, Network: "echofog-v2", Joined: []JoinedPeer{}}
	for _, j := range st.Joined {
		out.Joined = append(out.Joined, JoinedPeer{PeerID: j.Pass, JoinedAt: j.JoinedAt.Unix(), Connected: j.Connected})
	}
	return out, nil
}

// SetAccessMode saves the mode for the next start. The desktop offers public and hidden.
func (b *v2NetBackend) SetAccessMode(mode string) error {
	switch m := app.Mode(strings.TrimSpace(mode)); m {
	case app.ModePublic, app.ModeHidden:
		if b.check != nil {
			if err := b.check(m); err != nil {
				return err
			}
		}
		st, err := b.app.AccessStatus(context.Background())
		if err != nil {
			return v2Err(err)
		}
		if err := b.app.SetMode(m); err != nil {
			return v2Err(err)
		}
		// a changed mode applies by a restart of the runtime: the cards and the listeners are built once
		if b.restart != nil && st.Mode != m {
			if err := b.restart(); err != nil {
				return &v2UserError{msg: "Не удалось перезапустить сеть в новом режиме доступа"}
			}
		}
		return nil
	}
	return &v2UserError{msg: "Этот режим доступа недоступен в протоколе 2"}
}

func (b *v2NetBackend) ParseInvite(link string) (InviteInfo, error) {
	info, err := b.app.Preview(link)
	if err != nil {
		return InviteInfo{}, v2Err(err)
	}
	return InviteInfo{PeerID: info.Node, Private: info.Kind == "invitation", Protocol: 2}, nil
}

// JoinInvite joins by a link of either kind and returns the contact's node ID.
func (b *v2NetBackend) JoinInvite(link string) (string, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	c, err := b.app.AddByLink(ctx, link)
	if err != nil {
		return "", v2Err(err)
	}
	return c.Node, nil
}

func (b *v2NetBackend) GetPeers() []dto.PeerDTO {
	st := b.app.NetStatus()
	out := make([]dto.PeerDTO, 0, len(st.Peers))
	for _, p := range st.Peers {
		out = append(out, dto.PeerDTO{PeerID: p.Device, ShortID: p.Device, ProtocolVersion: 2, IsConnected: true})
	}
	return out
}

func (b *v2NetBackend) GetNetworkStats() NetworkStats {
	st := b.app.NetStatus()
	return NetworkStats{Online: st.Running && st.Sessions > 0, ConnectedPeers: st.Sessions}
}

// GetGuardStatus reports the route policy class as the mode and the sessions that run through a
// relay as the held guards: v2 has no guard list of the v1 kind.
func (b *v2NetBackend) GetGuardStatus() (GuardStatus, error) {
	st := b.app.NetStatus()
	return GuardStatus{Mode: st.NetClass, Active: st.Running, Connected: st.Relay, Guards: []GuardServer{}, Pinned: []BootstrapNode{}}, nil
}

func passFromApp(p app.PassView, link string) PassView {
	state := p.State
	if state == "used" {
		state = "used_up"
	}
	v := PassView{ID: p.ID, Link: link, State: state, Created: p.Created.Unix(), MaxDevices: int(p.MaxEntries), Devices: []coreNode.PassDevice{}}
	if !p.AccessExpires.IsZero() {
		v.Expires = p.AccessExpires.Unix()
	} else if !p.LinkExpires.IsZero() {
		v.Expires = p.LinkExpires.Unix()
	}
	return v
}

func (b *v2NetBackend) ListPasses() ([]PassView, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	list, err := b.app.Passes(ctx)
	if err != nil {
		return nil, v2Err(err)
	}
	out := make([]PassView, 0, len(list))
	for _, p := range list {
		out = append(out, passFromApp(p, ""))
	}
	return out, nil
}

// CreatePass mints an invitation of a hidden node: Days is the access term after the first entry,
// Devices how many people can use the link. The link is shown in the result once.
func (b *v2NetBackend) CreatePass(r PassRequest) (PassView, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	inv, err := b.app.Invite(ctx, app.InviteRequest{Days: r.Days, Entries: uint32(max(r.Devices, 0))})
	if err != nil {
		return PassView{}, v2Err(err)
	}
	v := PassView{ID: inv.Pass, Link: inv.Link, WebLink: inv.Web, State: "active", Created: time.Now().Unix(),
		Expires: inv.Expires.Unix(), MaxDevices: int(inv.Entries), Label: r.Label, Devices: []coreNode.PassDevice{}}
	return v, nil
}

func (b *v2NetBackend) ExtendPass(id string, r PassRequest) (PassView, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	p, err := b.app.ExtendPass(ctx, id, r.Days)
	if err != nil {
		return PassView{}, v2Err(err)
	}
	return passFromApp(p, ""), nil
}

func (b *v2NetBackend) RevokePass(id string) (PassView, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	p, err := b.app.RevokePass(ctx, id)
	if err != nil {
		return PassView{}, v2Err(err)
	}
	return passFromApp(p, ""), nil
}

// RemoveJoinedPeer revokes the pass of somebody who came in (their ID in the list is the pass ID).
func (b *v2NetBackend) RemoveJoinedPeer(peerID string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	_, err := b.app.RevokePass(ctx, peerID)
	return v2Err(err)
}

// RotateAccessSecret rotates the masking keys. Nothing secret is returned to the UI.
func (b *v2NetBackend) RotateAccessSecret() (string, error) {
	ctx, cancel := v2Ctx()
	defer cancel()
	return "", v2Err(b.app.RotateAccessKeys(ctx))
}

func (b *v2ContactBackend) GetContacts() ([]dto.ContactDTO, error) {
	list, err := b.app.Contacts()
	if err != nil {
		return nil, v2Err(err)
	}
	out := make([]dto.ContactDTO, 0, len(list))
	for _, c := range list {
		if c.Blocked {
			continue
		}
		out = append(out, dto.ContactDTO{PeerID: c.Node, PubKey: c.Node, Alias: c.Alias})
	}
	return out, nil
}

func (b *v2ContactBackend) SetContactProfile(peerID, alias, avatarBase64 string) error {
	return v2Err(b.app.SetAlias(peerID, alias))
}

func (b *v2ContactBackend) DeleteContact(peerID string) error {
	return v2Err(b.app.DeleteContact(peerID))
}

func (b *v2ContactBackend) BlockPeer(peerID string) error   { return v2Err(b.app.Block(peerID)) }
func (b *v2ContactBackend) UnblockPeer(peerID string) error { return v2Err(b.app.Unblock(peerID)) }

func (b *v2ContactBackend) GetBlockedPeers() ([]contacts.Blocked, error) {
	list, err := b.app.Contacts()
	if err != nil {
		return nil, v2Err(err)
	}
	out := []contacts.Blocked{}
	for _, c := range list {
		if c.Blocked {
			out = append(out, contacts.Blocked{PeerID: c.Node, Alias: c.Alias, At: c.Added.Unix()})
		}
	}
	return out, nil
}

var (
	_ netBackend     = (*v2NetBackend)(nil)
	_ contactBackend = (*v2ContactBackend)(nil)
)

// ConnectionReport of a protocol 2 node is the report of the app layer: the same header and no
// addresses, IDs, links or contacts.
func (b *v2NetBackend) connectionReport(appVersion string) ConnectionReport {
	return ConnectionReport{Text: b.app.Report(), Version: appVersion}
}
