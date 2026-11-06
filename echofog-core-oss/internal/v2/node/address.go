package node

import (
	"net/netip"

	"github.com/echofog/echofog-core/internal/v2/session"
)

// PublicAddr returns the node's public IP as decided by agreement of authenticated peers
// (OBSERVED_ADDR, wire.md §7.2). There is none until at least two peers in different /16
// networks agree. The node never sends STUN from a carrier socket (D20).
func (r *Runtime) PublicAddr() (netip.Addr, bool) { return r.observer.PublicAddr() }

// NATClass is the NAT class that peers' reports imply, for path planning (T6.2): unknown until
// three peers in different networks have reported on one local socket.
func (r *Runtime) NATClass() session.NATClass { return r.observer.NATClass() }

// publicAddrChanged is the address-change event: the cards follow the new public IP (the next
// Current reissues them) and the operator is told.
func (r *Runtime) publicAddrChanged(c session.PublicAddrChange) {
	r.noteCountry()     // the confirmed address may decide the country (T6.4c)
	r.pm.Confirm(c.New) // peers vouch for the IP: a mapping on it becomes a candidate (T6.5b)
	_, err := r.cfg.Cards.SetPublicHost(c.New.String())
	if err != nil {
		r.log.Warn("node: cards did not follow the public address", "err", err)
	}
	msg := "public address decided"
	if c.Old.IsValid() {
		msg = "public address changed"
	}
	r.events.Emit(Event{
		Kind:       EventPublicAddr,
		Message:    msg,
		At:         r.clock.Now(),
		Err:        err,
		PublicAddr: c.New,
	})
}
