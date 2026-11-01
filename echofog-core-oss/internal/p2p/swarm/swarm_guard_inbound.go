package swarm

import (
	"errors"
	"net"
	"net/netip"
	"sync/atomic"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// inboundInvited reports whether a peer came in with this node's invitation (SetInboundInvited).
type inboundInvited = atomic.Pointer[func(types.PeerID) bool]

// SetInboundInvited tells the swarm which peers the owner let in with an invitation link or a
// pass: guard mode keeps taking them directly from across the border (the owner's phone abroad
// using the home computer as its exit). nil: none.
func (s *Swarm) SetInboundInvited(fn func(types.PeerID) bool) {
	if fn == nil {
		s.invited.Store(nil)
		return
	}
	s.invited.Store(&fn)
}

func (s *Swarm) isInvitedPeer(id types.PeerID) bool {
	fn := s.invited.Load()
	return fn != nil && (*fn)(id)
}

// errInboundAcrossBorder: a node in guard mode does not take a direct link from across the border.
var errInboundAcrossBorder = errors.New("guard mode: no direct link from across the border")

type listenHider interface {
	SetHideListenWhen(fn func(remote net.Addr) bool)
}

// hideListenInGuardMode makes the transport announce no listen port to peers outside the local
// network while guard mode is on, so the peers this node dials (its guards first, its own
// country's nodes too) do not spread its address. A node with a public address in a censored
// country would otherwise be dialed straight from across the border: a noise link a censor can
// flag, made before this node can refuse it. A peer on the local network sees only a local
// address, which is of no use abroad.
func (s *Swarm) hideListenInGuardMode(t any) {
	if h, ok := t.(listenHider); ok {
		h.SetHideListenWhen(func(remote net.Addr) bool {
			return s.GuardModeActive() && !onLocalNetwork(remote.String())
		})
	}
}

// inboundAcrossBorderRefused reports whether an inbound peer must be turned away: this node is in
// guard mode, the link came from across the border, and the peer is not one it keeps a direct
// link with anyway (a guard, a server, a peer the user listed or invited). Such a link is noise between a
// censored node and a node abroad, the traffic guard mode keeps off the border; the peer reaches
// this node through a relay instead (UserLinkViaRelay is the same rule for dials).
func (s *Swarm) inboundAcrossBorderRefused(ev network.NewConnEvent) bool {
	if !s.GuardModeActive() || !ev.RemoteIP.IsValid() {
		return false
	}
	id := ev.PeerID
	if s.isExplicitPeer(id) || s.isInvitedPeer(id) || s.IsGuard(id) || s.IsServerPeer(id) || ev.Caps.Has(handshake.CapServer) {
		return false
	}
	return s.acrossBorder(netip.AddrPortFrom(ev.RemoteIP.Unmap(), 0).String())
}
