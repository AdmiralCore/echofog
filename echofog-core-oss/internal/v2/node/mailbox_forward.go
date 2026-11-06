package node

import (
	"context"
	"errors"
	"net/netip"

	"github.com/echofog/echofog-core/internal/v2/mailbox"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
)

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// relayCardAddrOK says whether F may dial an address of a relay card the sender supplied. Only public
// unicast addresses pass (services.md 5.3); a test replaces it to use in-memory carriers.
var relayCardAddrOK = relayCardAddrDefault

func relayCardAddrDefault(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !cgnatPrefix.Contains(ip)
}

// relayCardUsable reports whether F may dial the card of a relay it was given by a sender: every address is
// an IP literal that passes relayCardAddrOK (a host name would be resolved by the dial, to anything).
func relayCardUsable(card *objects.Card) bool {
	addrs := card.Addrs()
	if len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		ip, err := netip.ParseAddr(a.GetHost())
		if err != nil || !relayCardAddrOK(ip) {
			return false
		}
	}
	return true
}

// mailboxForwarder makes this node a forwarding relay F (services.md 5.3): it reaches the mailbox relay
// over an existing session, or dials it only with a card it verified itself or a relay card signed by
// the relay, at public addresses.
type mailboxForwarder struct{ r *Runtime }

func (f mailboxForwarder) Connected(relay []byte) bool {
	if len(relay) != 32 {
		return false
	}
	return f.r.liveMainSession(DeviceID(relay)) != nil
}

func (f mailboxForwarder) Forward(ctx context.Context, m mailbox.Forward) (uint32, error) {
	if len(m.Relay) != 32 {
		return mailbox.StatusRelayUnreachable, nil
	}
	dev := DeviceID(m.Relay)
	var opt DialOpt
	if f.r.liveMainSession(dev) == nil {
		card := f.r.cardBook.Latest([32]byte(dev))
		if card == nil && len(m.RelayCard) > 0 {
			c, err := objects.VerifyCard(m.RelayCard, f.r.clock.Now())
			if err == nil && c.DeviceID == [32]byte(dev) && relayCardUsable(c) {
				card = c
			}
		}
		if card == nil {
			return mailbox.StatusRelayUnreachable, nil
		}
		opt = DialOpt{Card: card}
		if hook := f.r.mailboxDialHook; hook != nil {
			opt = hook(card)
		}
	}
	p, st, err := f.r.Open(ctx, dev, service.Mailbox, opt)
	if err != nil {
		return mailbox.StatusRelayUnreachable, nil
	}
	defer st.Close()
	conn := mailbox.NewStreamConn(st, p.SID, nil)
	// the relay card stays at F: M needs only the sealed deposit
	d, err := conn.Forward(ctx, mailbox.Forward{Req: m.Req, Relay: m.Relay, Sealed: m.Sealed})
	if err != nil {
		var re *mailbox.RemoteError
		if errors.As(err, &re) {
			return mailbox.StatusRefused, nil
		}
		return 0, err
	}
	return d.Status, nil
}
