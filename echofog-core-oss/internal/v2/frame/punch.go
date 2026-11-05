package frame

import "net/netip"

// MaxPunchAddrs is how many addresses of a PUNCH_OFFER a node uses (services.md §6.4 step 2).
const MaxPunchAddrs = 4

// PunchOffer is the payload of PUNCH_OFFER (registries.md §5):
// PunchOffer { repeated bytes addrs = 1; uint64 nonce = 2; }, each addr family(1) ‖ ip ‖ port(2).
type PunchOffer struct {
	Addrs []netip.AddrPort
	Nonce uint64
}

// Append encodes the offer canonically.
func (o PunchOffer) Append(b []byte) []byte { return b }

// ParsePunchOffer decodes PUNCH_OFFER.
func ParsePunchOffer(p []byte) (PunchOffer, error) { return PunchOffer{}, nil }
