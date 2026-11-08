package node

import (
	"context"
	"fmt"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/app"
)

// v2VPN is the part of *app.App the protocol 2 VPN backend uses (T11.4).
type v2VPN interface {
	VPNStart(ctx context.Context, link string) (app.VPNStatus, error)
	VPNStartSaved(ctx context.Context, node string) (app.VPNStatus, error)
	VPNStop() error
	VPNStatus() app.VPNStatus
	VPNExits() []app.SavedExit
	VPNHistory() []app.VPNSession
	VPNClearHistory() error
	VPNForget(node string) error
	VPNName(node, name string) error
	VPNFavorite(node string, on bool) error
}

var _ v2VPN = (*app.App)(nil)

// v2VPNBackend is the protocol 2 VPN backend: a local proxy through a free exit that a link names.
// The market, payment, the probes, the whole-computer mode and the automatic switch are not part of
// it and answer as the pending backend does.
type v2VPNBackend struct {
	v2PendingBackend
	app v2VPN
}

var _ vpnBackend = (*v2VPNBackend)(nil)

// v2NetVPN adds the start by a link to the network backend: StartVPNVia belongs to the network domain
// of the facade, but it is the VPN that runs.
type v2NetVPN struct {
	netBackend
	vpn *v2VPNBackend
}

// AttachV2VPN gives the facade the v2 VPN: the methods of the free VPN answer from the app layer from
// now on, and StartVPNVia starts the exit of a link. Call it after AttachV2. At protocol 1 it does
// nothing.
func (n *NodeApi) AttachV2VPN(a v2VPN) {
	if a == nil || n.node == nil || n.node.Cfg == nil || !n.node.Cfg.IsV2() {
		return
	}
	b := &v2VPNBackend{app: a}
	n.backends.vpn = b
	n.backends.net = &v2NetVPN{netBackend: n.backends.net, vpn: b}
	n.v2v.mu.Lock()
	n.v2v.b = b
	n.v2v.mu.Unlock()
}

// v2VPNHolder keeps the attached VPN backend for the facade methods that route to it.
type v2VPNHolder struct {
	mu sync.Mutex
	b  *v2VPNBackend
}

// v2vpn is the attached protocol 2 VPN backend, nil at protocol 1 or before AttachV2VPN.
func (n *NodeApi) v2vpn() *v2VPNBackend {
	n.v2v.mu.Lock()
	defer n.v2v.mu.Unlock()
	return n.v2v.b
}

// ---- the methods ---------------------------------------------------------------------------------------------

// StartVPNVia starts the VPN through the exit a link names.
func (b *v2NetVPN) StartVPNVia(target string) error { return b.vpn.startVia(target) }

func (b *v2VPNBackend) startVia(link string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	_, err := b.app.VPNStart(ctx, link)
	return v2Err(err)
}

// StartVPN connects to an exit of the list (the argument is its node ID).
func (b *v2VPNBackend) StartVPN(relayPeerID string) error {
	ctx, cancel := v2Ctx()
	defer cancel()
	_, err := b.app.VPNStartSaved(ctx, relayPeerID)
	return v2Err(err)
}

func (b *v2VPNBackend) StopVPN() error { return v2Err(b.app.VPNStop()) }

func (b *v2VPNBackend) StopVPNForSwitch() error { return v2Err(b.app.VPNStop()) }

// ConnectLastExit connects to the exit used last.
func (b *v2VPNBackend) ConnectLastExit() error {
	var last *app.SavedExit
	for _, e := range b.app.VPNExits() {
		if last == nil || e.LastUsed > last.LastUsed {
			e := e
			last = &e
		}
	}
	if last == nil {
		return &v2UserError{msg: "Нет выхода, к которому можно подключиться: откройте ссылку выхода"}
	}
	return b.StartVPN(last.Node)
}

func (b *v2VPNBackend) GetVPNStatus() VPNStatus {
	st := b.app.VPNStatus()
	if !st.Running {
		return VPNStatus{}
	}
	return VPNStatus{Running: true, ListenAddr: st.Listen, RelayPeerID: st.Exit, ActiveConns: st.Active, BytesRx: st.Rx, BytesTx: st.Tx,
		TunnelReady: st.ExitState == app.ExitOK, ExitState: st.ExitState, Refused: st.Refused, LastRefusalReason: st.LastReason,
		LastRefusalHost: st.LastHost, LastRefusalAt: st.LastAt}
}

func savedExit(e app.SavedExit) SavedExit {
	// every exit is free here: the price is known and zero
	return SavedExit{PeerID: e.Node, Name: e.Name, Favorite: e.Favorite, Active: e.Active, Sessions: e.Sessions, LastUsed: e.LastUsed,
		BytesRx: e.Rx, BytesTx: e.Tx, Refused: e.Refused, RefusedBy: e.RefusedBy, PriceKnown: true}
}

func (b *v2VPNBackend) GetSavedExits() []SavedExit {
	list := b.app.VPNExits()
	out := make([]SavedExit, 0, len(list))
	for _, e := range list {
		out = append(out, savedExit(e))
	}
	return out
}

// ListExitNodes lists the exits the user has: there is no market, so these are the saved ones.
func (b *v2VPNBackend) ListExitNodes() []ExitNode {
	list := b.app.VPNExits()
	out := make([]ExitNode, 0, len(list))
	for _, e := range list {
		out = append(out, ExitNode{PeerID: e.Node, LastProbe: e.LastUsed, Connected: e.Active})
	}
	return out
}

func (b *v2VPNBackend) SetExitName(peer, name string) error { return v2Err(b.app.VPNName(peer, name)) }

func (b *v2VPNBackend) SetExitFavorite(peer string, on bool) error {
	return v2Err(b.app.VPNFavorite(peer, on))
}

func (b *v2VPNBackend) ForgetExit(peer string) error { return v2Err(b.app.VPNForget(peer)) }

func (b *v2VPNBackend) GetVPNHistory() []VPNSession {
	list := b.app.VPNHistory()
	out := make([]VPNSession, 0, len(list))
	for _, s := range list {
		out = append(out, VPNSession{ID: fmt.Sprintf("%s-%d", s.Node[:min(8, len(s.Node))], s.StartedAt), PeerID: s.Node, StartedAt: s.StartedAt, EndedAt: s.EndedAt,
			UpdatedAt: max(s.EndedAt, s.StartedAt), BytesRx: s.Rx, BytesTx: s.Tx, Refused: s.Refused, RefusedBy: s.RefusedBy, PriceKnown: true,
			EndReason: s.EndReason})
	}
	return out
}

func (b *v2VPNBackend) ClearVPNHistory() error { return v2Err(b.app.VPNClearHistory()) }
