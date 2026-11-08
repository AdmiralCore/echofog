package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/tun"
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

// v2Whole is the whole-computer mode of *app.App (T10.6b). It is a separate interface: an app built
// without drivers answers WholeComputerSupported() false and the backend then behaves as before.
type v2Whole interface {
	WholeComputerSupported() bool
	WholeComputerStart(ctx context.Context) error
	WholeComputerStop(ctx context.Context) error
	WholeComputerRecover(ctx context.Context) error
	WholeComputerStatus() app.WholeStatus
	VPNStopForSwitch() error
}

var _ v2Whole = (*app.App)(nil)

// wholeStartTimeout covers the prompt for administrator rights.
const wholeStartTimeout = 3 * time.Minute

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
	// Filters of an earlier run that crashed hold the computer closed; the screen must show that
	// before the user touches anything.
	if w, ok := b.whole(); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = w.WholeComputerRecover(ctx)
	}
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

// StopVPNForSwitch stops the VPN to start another exit; the whole-computer mode, if it is on, stays
// on, so nothing gets out between the two.
func (b *v2VPNBackend) StopVPNForSwitch() error {
	if w, ok := b.whole(); ok {
		return v2Err(w.VPNStopForSwitch())
	}
	return v2Err(b.app.VPNStop())
}

// whole is the whole-computer mode when the app has it.
func (b *v2VPNBackend) whole() (v2Whole, bool) {
	w, ok := b.app.(v2Whole)
	if !ok || !w.WholeComputerSupported() {
		return nil, false
	}
	return w, true
}

func (b *v2VPNBackend) WholeComputerSupported() bool {
	_, ok := b.whole()
	return ok
}

// SetWholeComputer turns the whole-computer mode with its kill switch on or off. Turning it off is
// also the "Disable protection" of the interface: from the held state it removes the filters.
func (b *v2VPNBackend) SetWholeComputer(on bool) error {
	w, ok := b.whole()
	if !ok {
		return ErrV2NotBuilt
	}
	ctx, cancel := context.WithTimeout(context.Background(), wholeStartTimeout)
	defer cancel()
	if on {
		return v2Err(w.WholeComputerStart(ctx))
	}
	return v2Err(w.WholeComputerStop(ctx))
}

// KillSwitchOn: the kill switch is part of the mode and on as long as the mode is.
func (b *v2VPNBackend) KillSwitchOn() bool {
	w, ok := b.whole()
	return ok && w.WholeComputerStatus().State != tun.StateOff
}

// SetKillSwitch accepts what is already true and refuses to open the computer inside the mode: the
// way out is to turn the mode off.
func (b *v2VPNBackend) SetKillSwitch(on bool) error {
	if _, ok := b.whole(); !ok {
		return ErrV2NotBuilt
	}
	if !on {
		return &v2UserError{msg: "В режиме «весь компьютер» защита всегда включена: чтобы выйти в обычную сеть, отключите защиту"}
	}
	return nil
}

// withWhole adds the whole-computer mode to a VPN status. A held mode blocks the computer whether
// the VPN runs or not, and is shown either way.
func (b *v2VPNBackend) withWhole(st VPNStatus) VPNStatus {
	w, ok := b.whole()
	if !ok {
		return st
	}
	ws := w.WholeComputerStatus()
	st.WholeComputerSupported = true
	st.WholeComputer = ws.State != tun.StateOff
	st.KillSwitch = ws.State != tun.StateOff
	st.Blocked = ws.State == tun.StateHeld
	switch {
	case ws.State == tun.StateHeld:
		st.WholeComputerError = "Туннель потерян: интернет заблокирован, пока защита не отключена или туннель не восстановлен"
	case ws.Err != "":
		st.WholeComputerError = "Не удалось включить режим «весь компьютер»"
	}
	return st
}

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
		return b.withWhole(VPNStatus{})
	}
	return b.withWhole(VPNStatus{Running: true, ListenAddr: st.Listen, RelayPeerID: st.Exit, ActiveConns: st.Active, BytesRx: st.Rx, BytesTx: st.Tx,
		TunnelReady: st.ExitState == app.ExitOK, ExitState: st.ExitState, Refused: st.Refused, LastRefusalReason: st.LastReason,
		LastRefusalHost: st.LastHost, LastRefusalAt: st.LastAt})
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
