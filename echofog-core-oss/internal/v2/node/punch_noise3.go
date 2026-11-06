package node

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/punch"
)

// Noise3Punch is the built-in PunchNetwork: junk and Init datagrams leave from the socket of the
// node's noise3 listener (Noise3Listener.Punch fills Listener), so the NAT mapping the peer must hit
// is the listener's. OwnAddrs is the public IP the peers agreed on (T3.4) with the listener's port;
// that is exact behind a cone NAT that keeps ports and behind a mapped port (T6.5), and the Observer
// corrects the IP, not the port, until carriers expose their local socket.
type Noise3Punch struct {
	mu       sync.Mutex
	listener *noise3.Listener
	rt       *Runtime

	// PublicIP overrides the Observer's decision (tests, a mapped address).
	PublicIP func() (netip.Addr, bool)
	// Card overrides the card lookup of the peer (tests); the runtime's card book is the default.
	Card func(DeviceID) *objects.Card
}

var _ PunchNetwork = (*Noise3Punch)(nil)

func (p *Noise3Punch) attach(l *noise3.Listener) {
	p.mu.Lock()
	p.listener = l
	p.mu.Unlock()
}

func (p *Noise3Punch) bind(r *Runtime) {
	p.mu.Lock()
	p.rt = r
	p.mu.Unlock()
}

func (p *Noise3Punch) own() []netip.AddrPort {
	p.mu.Lock()
	l, rt := p.listener, p.rt
	p.mu.Unlock()
	if l == nil {
		return nil
	}
	ua, ok := l.Addr().(*net.UDPAddr)
	if !ok {
		return nil
	}
	var ip netip.Addr
	var have bool
	switch {
	case p.PublicIP != nil:
		ip, have = p.PublicIP()
	case rt != nil:
		ip, have = rt.PublicAddr()
	}
	if !have {
		return nil
	}
	return []netip.AddrPort{netip.AddrPortFrom(ip.Unmap(), uint16(ua.Port))}
}

// Net implements PunchNetwork.
func (p *Noise3Punch) Net(peer DeviceID, _ punch.Role) punch.Net {
	p.mu.Lock()
	l, rt := p.listener, p.rt
	p.mu.Unlock()
	var card *objects.Card
	switch {
	case p.Card != nil:
		card = p.Card(peer)
	case rt != nil:
		card = rt.cardBook.Latest([32]byte(peer))
	}
	if l == nil {
		return unavailableNet{}
	}
	return &noise3.PunchNet{Listener: l, Own: p.own, Card: card}
}

// unavailableNet is the datagram side of a node without a noise3 listener: it offers nothing.
type unavailableNet struct{}

func (unavailableNet) OwnAddrs() []netip.AddrPort { return nil }
func (unavailableNet) SendJunk(netip.AddrPort, []byte) error {
	return errors.New("node: no noise3 listener")
}
func (unavailableNet) Dial(context.Context, []netip.AddrPort, int) (mux.Carrier, error) {
	return nil, errors.New("node: no noise3 listener")
}

var _ BirthdayNetwork = (*Noise3Punch)(nil)

// Socks implements BirthdayNetwork: extra UDP sockets for the symmetric side, the listener's socket
// for the spray, Init and accept on the socket a mapping was found for (T6.7).
func (p *Noise3Punch) Socks(peer DeviceID, deliver func(mux.Carrier)) punch.SockNet {
	p.mu.Lock()
	l, rt := p.listener, p.rt
	p.mu.Unlock()
	if l == nil {
		return nil
	}
	var card *objects.Card
	switch {
	case p.Card != nil:
		card = p.Card(peer)
	case rt != nil:
		card = rt.cardBook.Latest([32]byte(peer))
	}
	return &noise3.BirthdaySocks{Main: l, Card: card, Deliver: deliver}
}
