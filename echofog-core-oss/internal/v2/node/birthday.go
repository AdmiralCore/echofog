package node

import (
	"net/netip"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/punch"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// BirthdayGates are the conditions of services.md section 6.4.1 for one session: noise3 allowed to
// the peer, the feature negotiated, and the node's own modes and profile flag. Low power (battery
// saver) turns the flag off.
func BirthdayGates(p Profile, proxy, anonymous, noise3Allowed, negotiated bool) punch.Gates {
	return punch.Gates{
		Noise3Allowed: noise3Allowed,
		Negotiated:    negotiated,
		Guard:         p.Guard,
		Anonymous:     anonymous,
		Proxy:         proxy,
		Flag:          p.Birthday && !p.LowPower,
	}
}

// BirthdayNetwork is the optional part of a PunchNetwork that supplies the sockets of birthday
// punching for one peer. deliver receives a carrier accepted on a birthday socket.
type BirthdayNetwork interface {
	Socks(peer DeviceID, deliver func(mux.Carrier)) punch.SockNet
}

// natInfo is this node's NAT as the punch engine states it: the class peers' reports imply and, for
// a symmetric NAT, the port range they named.
func (r *Runtime) natInfo() punch.NATInfo {
	d := r.observer.NATDetail()
	n := punch.NATInfo{Lo: d.Lo, Hi: d.Hi, Samples: d.Samples}
	switch d.Class {
	case session.NATCone:
		n.Class = punch.NATCone
	case session.NATSymmetric:
		n.Class = punch.NATSymmetric
	default:
		n.Class = punch.NATUnknown
	}
	return n
}

// birthdayFor returns the Birthday of one relayed session, or nil when the network supplies no
// birthday sockets.
func (c *relayClient) birthdayFor(ps *punchState) *punch.Birthday {
	r := c.r
	bn, ok := r.cfg.PunchNet.(BirthdayNetwork)
	if !ok || ps.est == nil || ps.est.Peer == nil {
		return nil
	}
	socks := bn.Socks(DeviceID(ps.est.Peer.DeviceID), r.serveBirthdayCarrier)
	if socks == nil {
		return nil
	}
	est := ps.est
	return punch.NewBirthday(punch.BirthdayConfig{
		Clock:  r.carrierClock,
		Net:    socks,
		Shared: r.bd,
		Gates: func() punch.Gates {
			neg := est.HasFeature(frame.FeatureBirthdayPunch) && est.HasFeature(frame.FeaturePathMigration)
			return BirthdayGates(r.cfg.Profile, r.cfg.DialContext != nil, r.cfg.Cards.Visibility() == cards.Anonymous,
				c.allowed(ps), neg)
		},
	})
}

// serveBirthdayCarrier handles a carrier accepted on a birthday socket like one from a listener:
// bounded handshakes, then the ordinary accept path (a HS_HELLO or the PATH_BIND of the punch).
func (r *Runtime) serveBirthdayCarrier(c mux.Carrier) {
	select {
	case r.hsSem <- struct{}{}:
	default:
		_ = c.Close()
		return
	}
	if !r.track() {
		<-r.hsSem
		_ = c.Close()
		return
	}
	go func() {
		defer r.wg.Done()
		defer func() { <-r.hsSem }()
		r.serveCarrierAs(r.ctx, c, mux.PathDirect, netip.AddrPort{})
	}()
}

// BirthdayStats returns the birthday attempts and the ones that found a mapping (counts only).
func (r *Runtime) BirthdayStats() (tried, ok int64) { return r.bd.Stats() }

