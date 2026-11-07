package node

import (
	"context"
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
