// Package relay implements relay/2 (docs/protocol/v2/services.md §6, service 5): circuits through
// a relay R between two nodes A and B that are both connected to R.
//
// Inside a circuit A and B run their own end-to-end handshake, so R carries only records
// (wire.md §8.4) it can neither read nor change. R copies bytes after CONNECTED and ACCEPT;
// everything R decides lives before that point (who may connect to whom, limits, refusals).
//
// The package does not import the v1 network (AD1) nor the node runtime: the server takes a
// Directory of the nodes connected to it, the client takes the stream opener of its session with
// R, and the circuit is handed to the session layer as a mux.Carrier.
package relay

import (
	"errors"
	"fmt"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

// Circuit modes (services.md §6.2).
const (
	ModeStream   uint32 = 1
	ModeDatagram uint32 = 2
	ModeSignal   uint32 = 3
)

// Target kinds (services.md §6.3).
const (
	TargetNode       uint32 = 0
	TargetRendezvous uint32 = 1
)

// REFUSED reasons (services.md §6.3). Every refusal about the target is Unavailable.
const (
	ReasonUnavailable uint32 = 1
	ReasonLimit       uint32 = 2
)

// Frame types of relay/2 (services.md §6.1).
const (
	TypeConnect    uint64 = 0x40
	TypeIncoming   uint64 = 0x41
	TypeAccept     uint64 = 0x42
	TypeRefuse     uint64 = 0x43
	TypeConnected  uint64 = 0x44
	TypeRefused    uint64 = 0x45
	TypeRegister   uint64 = 0x46
	TypeRegistered uint64 = 0x47
	TypePeerAddr   uint64 = 0x48
)

// Version is the relay service version this package speaks.
const Version uint64 = 2

// MaxRegisterIDs is the most rendezvous IDs one REGISTER carries (and one node holds at R).
const MaxRegisterIDs = 256

// MaxCID is the largest circuit ID: RELAY_DGRAM carries it as a QUIC varint (62 bits), so R draws
// its random 64-bit value from the 62-bit range and a node refuses a larger one (spec question).
const MaxCID = 1<<62 - 1

const idLen = 32

// ErrMalformed marks a payload that is not a valid relay/2 frame. The stream that carried it is
// closed with a protocol violation; R never answers a malformed frame with a refusal that could
// differ by target.
var ErrMalformed = errors.New("relay: malformed frame")

func malformed(what string) error { return fmt.Errorf("%w: %s", ErrMalformed, what) }

// Connect is CONNECT (A → R).
type Connect struct {
	Req        uint64
	Target     []byte
	Mode       uint32
	Token      []byte
	TargetKind uint32
	WantAddr   bool
}

// Incoming is INCOMING (R → B).
type Incoming struct {
	CID      uint64
	Source   []byte
	Mode     uint32
	Target   []byte
	WantAddr bool
}

// Accept is ACCEPT (B → R).
type Accept struct {
	CID      uint64
	WantAddr bool
}

// Refuse is REFUSE (B → R).
type Refuse struct{ CID uint64 }

// Connected is CONNECTED (R → A).
type Connected struct {
	Req    uint64
	CID    uint64
	Target []byte
}

// Refused is REFUSED (R → A).
type Refused struct {
	Req    uint64
	Target []byte
	Reason uint32
}

// Register is REGISTER (B → R).
type Register struct {
	Req uint64
	IDs [][]byte
	Day uint32
}

// Registered is REGISTERED (R → B).
type Registered struct {
	Req   uint64
	Count uint32
}

// PeerAddr is PEER_ADDR (R → A, B).
type PeerAddr struct {
	CID  uint64
	Addr []byte
}

func putUint(b []byte, num protowire.Number, v uint64) []byte {
	if v == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func putBytes(b []byte, num protowire.Number, v []byte) []byte {
	if len(v) == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func putBool(b []byte, num protowire.Number, v bool) []byte {
	if !v {
		return b
	}
	return putUint(b, num, 1)
}

// walk calls f for each field. Fields must come in strictly ascending order (canonical
// encoding, as every other v2 service requires); unknown numbers are passed to f too, which
// ignores them (versioning.md §3.5).
func walk(p []byte, f func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error) error {
	var last protowire.Number
	for len(p) > 0 {
		num, typ, n := protowire.ConsumeTag(p)
		if n < 0 || num < 1 {
			return malformed("tag")
		}
		if num <= last {
			return malformed("field order")
		}
		last = num
		p = p[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(p)
			if n < 0 {
				return malformed("varint")
			}
			p = p[n:]
			if err := f(num, typ, v, nil); err != nil {
				return err
			}
		case protowire.BytesType:
			s, n := protowire.ConsumeBytes(p)
			if n < 0 {
				return malformed("bytes")
			}
			p = p[n:]
			if err := f(num, typ, 0, s); err != nil {
				return err
			}
		default:
			return malformed("wire type")
		}
	}
	return nil
}

func u32(typ protowire.Type, v uint64, name string) (uint32, error) {
	if typ != protowire.VarintType || v > math.MaxUint32 {
		return 0, malformed(name)
	}
	return uint32(v), nil
}

func u64(typ protowire.Type, v uint64, name string) (uint64, error) {
	if typ != protowire.VarintType {
		return 0, malformed(name)
	}
	return v, nil
}

func flag(typ protowire.Type, v uint64, name string) (bool, error) {
	if typ != protowire.VarintType || v > 1 {
		return false, malformed(name)
	}
	return v == 1, nil
}

func id32(typ protowire.Type, s []byte, name string) ([]byte, error) {
	if typ != protowire.BytesType || len(s) != idLen {
		return nil, malformed(name)
	}
	return append([]byte(nil), s...), nil
}

func (c Connect) Append(b []byte) []byte {
	b = putUint(b, 1, c.Req)
	b = putBytes(b, 2, c.Target)
	b = putUint(b, 3, uint64(c.Mode))
	b = putBytes(b, 4, c.Token)
	b = putUint(b, 5, uint64(c.TargetKind))
	return putBool(b, 6, c.WantAddr)
}

func ParseConnect(p []byte) (c Connect, err error) {
	err = walk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		var e error
		switch num {
		case 1:
			c.Req, e = u64(typ, v, "connect.req")
		case 2:
			c.Target, e = id32(typ, s, "connect.target")
		case 3:
			c.Mode, e = u32(typ, v, "connect.mode")
		case 4:
			if typ != protowire.BytesType || len(s) > 64 {
				return malformed("connect.token")
			}
			c.Token = append([]byte(nil), s...)
		case 5:
			c.TargetKind, e = u32(typ, v, "connect.target_kind")
		case 6:
			c.WantAddr, e = flag(typ, v, "connect.want_addr")
		}
		return e
	})
	if err == nil && len(c.Target) != idLen {
		err = malformed("connect.target missing")
	}
	return
}

func (i Incoming) Append(b []byte) []byte {
	b = putUint(b, 1, i.CID)
	b = putBytes(b, 2, i.Source)
	b = putUint(b, 3, uint64(i.Mode))
	b = putBytes(b, 4, i.Target)
	return putBool(b, 5, i.WantAddr)
}

func ParseIncoming(p []byte) (i Incoming, err error) {
	err = walk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		var e error
		switch num {
		case 1:
			i.CID, e = u64(typ, v, "incoming.cid")
		case 2:
			i.Source, e = id32(typ, s, "incoming.source")
		case 3:
			i.Mode, e = u32(typ, v, "incoming.mode")
		case 4:
			i.Target, e = id32(typ, s, "incoming.target")
		case 5:
			i.WantAddr, e = flag(typ, v, "incoming.want_addr")
		}
		return e
	})
	return
}

func (a Accept) Append(b []byte) []byte {
	b = putUint(b, 1, a.CID)
	return putBool(b, 2, a.WantAddr)
}

func ParseAccept(p []byte) (a Accept, err error) {
	err = walk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		var e error
		switch num {
		case 1:
			a.CID, e = u64(typ, v, "accept.cid")
		case 2:
			a.WantAddr, e = flag(typ, v, "accept.want_addr")
		}
		return e
	})
	return
}

func (r Refuse) Append(b []byte) []byte { return putUint(b, 1, r.CID) }

func ParseRefuse(p []byte) (r Refuse, err error) {
	err = walk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) (e error) {
		if num == 1 {
			r.CID, e = u64(typ, v, "refuse.cid")
		}
		return
	})
	return
}

func (c Connected) Append(b []byte) []byte {
	b = putUint(b, 1, c.Req)
	b = putUint(b, 2, c.CID)
	return putBytes(b, 3, c.Target)
}

func ParseConnected(p []byte) (c Connected, err error) {
	err = walk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) (e error) {
		switch num {
		case 1:
			c.Req, e = u64(typ, v, "connected.req")
		case 2:
			c.CID, e = u64(typ, v, "connected.cid")
		case 3:
			c.Target, e = id32(typ, s, "connected.target")
		}
		return
	})
	return
}

func (r Refused) Append(b []byte) []byte {
	b = putUint(b, 1, r.Req)
	b = putBytes(b, 2, r.Target)
	return putUint(b, 3, uint64(r.Reason))
}

func ParseRefused(p []byte) (r Refused, err error) {
	err = walk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) (e error) {
		switch num {
		case 1:
			r.Req, e = u64(typ, v, "refused.req")
		case 2:
			r.Target, e = id32(typ, s, "refused.target")
		case 3:
			r.Reason, e = u32(typ, v, "refused.reason")
		}
		return
	})
	return
}

func (r Register) Append(b []byte) []byte {
	b = putUint(b, 1, r.Req)
	for _, id := range r.IDs {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, id)
	}
	return putUint(b, 3, uint64(r.Day))
}

// ParseRegister decodes REGISTER. The repeated field 2 is the one place a number repeats, so it
// does not use walk's ascending-order check.
func ParseRegister(p []byte) (r Register, err error) {
	var last protowire.Number
	for len(p) > 0 {
		num, typ, n := protowire.ConsumeTag(p)
		if n < 0 || num < 1 || num < last {
			return r, malformed("register tag")
		}
		last = num
		p = p[n:]
		switch {
		case typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(p)
			if n < 0 {
				return r, malformed("register varint")
			}
			p = p[n:]
			switch num {
			case 1:
				r.Req = v
			case 3:
				if v > math.MaxUint32 {
					return r, malformed("register.day")
				}
				r.Day = uint32(v)
			}
		case typ == protowire.BytesType:
			s, n := protowire.ConsumeBytes(p)
			if n < 0 {
				return r, malformed("register bytes")
			}
			p = p[n:]
			if num == 2 {
				if len(s) != idLen || len(r.IDs) >= MaxRegisterIDs {
					return r, malformed("register.ids")
				}
				r.IDs = append(r.IDs, append([]byte(nil), s...))
			}
		default:
			return r, malformed("register wire type")
		}
	}
	return r, nil
}

func (r Registered) Append(b []byte) []byte {
	b = putUint(b, 1, r.Req)
	return putUint(b, 2, uint64(r.Count))
}

func ParseRegistered(p []byte) (r Registered, err error) {
	err = walk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) (e error) {
		switch num {
		case 1:
			r.Req, e = u64(typ, v, "registered.req")
		case 2:
			r.Count, e = u32(typ, v, "registered.count")
		}
		return
	})
	return
}

func (a PeerAddr) Append(b []byte) []byte {
	b = putUint(b, 1, a.CID)
	return putBytes(b, 2, a.Addr)
}

func ParsePeerAddr(p []byte) (a PeerAddr, err error) {
	err = walk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) (e error) {
		switch num {
		case 1:
			a.CID, e = u64(typ, v, "peer_addr.cid")
		case 2:
			if typ != protowire.BytesType || len(s) > 19 {
				return malformed("peer_addr.addr")
			}
			a.Addr = append([]byte(nil), s...)
		}
		return
	})
	return
}
