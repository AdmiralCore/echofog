package frame

import (
	"net/netip"

	"google.golang.org/protobuf/encoding/protowire"
)

// MaxPunchAddrs is how many addresses of a PUNCH_OFFER a node uses (services.md §6.4 step 2).
const MaxPunchAddrs = 4

// PunchOffer is the payload of PUNCH_OFFER (registries.md §5):
// PunchOffer { repeated bytes addrs = 1; uint64 nonce = 2; }, each addr family(1) ‖ ip ‖ port(2).
type PunchOffer struct {
	Addrs []netip.AddrPort
	Nonce uint64
	// The birthday-punch additions (services.md §6.4.1), fields 3–7. Zero means "not stated": the
	// peer counts as a cone NAT and is never sprayed at.
	Nat     uint32
	PortLo  uint32
	PortHi  uint32
	Samples uint32
	Mode    uint32
}

// Append encodes the offer canonically: addresses in order, then the nonce (omitted when zero).
// At most MaxPunchAddrs addresses are written.
func (o PunchOffer) Append(b []byte) []byte {
	for i, a := range o.Addrs {
		if i == MaxPunchAddrs {
			break
		}
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, AppendObservedAddr(nil, a))
	}
	return pbUint(b, 2, o.Nonce)
}

// ParsePunchOffer decodes PUNCH_OFFER. Field numbers must not decrease (the only repeated field
// is 1), more than MaxPunchAddrs addresses are malformed, and unknown fields are skipped
// (versioning.md §3.5).
func ParsePunchOffer(p []byte) (PunchOffer, error) {
	var o PunchOffer
	var last protowire.Number
	for len(p) > 0 {
		num, typ, n := protowire.ConsumeTag(p)
		if n < 0 || num < 1 || num < last {
			return PunchOffer{}, malformed("punch_offer tag")
		}
		last = num
		p = p[n:]
		switch num {
		case 1:
			if typ != protowire.BytesType {
				return PunchOffer{}, malformed("punch_offer.addrs")
			}
			s, n := protowire.ConsumeBytes(p)
			if n < 0 || len(o.Addrs) >= MaxPunchAddrs {
				return PunchOffer{}, malformed("punch_offer.addrs")
			}
			p = p[n:]
			ap, err := ParseObservedAddr(s)
			if err != nil {
				return PunchOffer{}, err
			}
			o.Addrs = append(o.Addrs, ap)
		case 2:
			if typ != protowire.VarintType {
				return PunchOffer{}, malformed("punch_offer.nonce")
			}
			v, n := protowire.ConsumeVarint(p)
			if n < 0 {
				return PunchOffer{}, malformed("punch_offer.nonce")
			}
			p = p[n:]
			o.Nonce = v
		default:
			n := protowire.ConsumeFieldValue(num, typ, p)
			if n < 0 {
				return PunchOffer{}, malformed("punch_offer unknown field")
			}
			p = p[n:]
		}
	}
	return o, nil
}
