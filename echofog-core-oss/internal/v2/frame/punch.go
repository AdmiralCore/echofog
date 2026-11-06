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
	b = pbUint(b, 2, o.Nonce)
	b = pbUint(b, 3, uint64(o.Nat))
	b = pbUint(b, 4, uint64(o.PortLo))
	b = pbUint(b, 5, uint64(o.PortHi))
	b = pbUint(b, 6, uint64(o.Samples))
	return pbUint(b, 7, uint64(o.Mode))
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
		case 3, 4, 5, 6, 7:
			if typ != protowire.VarintType {
				return PunchOffer{}, malformed("punch_offer birthday field")
			}
			v, n := protowire.ConsumeVarint(p)
			if n < 0 || v > 0xffffffff {
				return PunchOffer{}, malformed("punch_offer birthday field")
			}
			p = p[n:]
			switch num {
			case 3:
				o.Nat = uint32(v)
			case 4:
				o.PortLo = uint32(v)
			case 5:
				o.PortHi = uint32(v)
			case 6:
				o.Samples = uint32(v)
			case 7:
				o.Mode = uint32(v)
			}
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
