// Package exit implements the exit/2 server of EchoFog protocol v2 (services.md §7): TCP
// tunnels, UDP tunnels over EXIT_UDP datagrams, account and voucher frames.
//
// The domain logic (target policy, DNS over HTTPS, quotas, traffic accounting, voucher book) is
// the v1 exit core, imported directly as AD16 requires. This package owns only the v2 wire
// format, the per-client-identity limits of §7 and the shape padding of the first records of a
// new tunnel (H1).
package exit

import (
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"google.golang.org/protobuf/encoding/protowire"
)

// Service-local frame types of exit/2 (services.md §7, registries.md §4).
const (
	TypeTCPConnect    uint64 = 0x40
	TypeResult        uint64 = 0x41
	TypeData          uint64 = 0x42
	TypeUDPOpen       uint64 = 0x43
	TypeUDPOK         uint64 = 0x44
	TypeUDPPkt        uint64 = 0x45
	TypeAccountGet    uint64 = 0x46
	TypeAccount       uint64 = 0x47
	TypeVoucher       uint64 = 0x48
	TypeVoucherResult uint64 = 0x49
)

// RESULT codes (registries.md §10.4).
const (
	ResultOK              uint32 = 0
	ResultDenied          uint32 = 1
	ResultUnreachable     uint32 = 2
	ResultPaymentRequired uint32 = 3
	ResultBusy            uint32 = 4
	ResultDNSFailed       uint32 = 5
	ResultBlockedPort     uint32 = 6
	ResultRateLimited     uint32 = 7
)

// Service-defined CLOSE codes of exit/2 (registries.md §10.2, services.md §7).
const (
	CodePaymentRequired uint64 = 0x100
	CodePolicy          uint64 = 0x101
)

// Bounds of the exit/2 bodies (services.md §7).
const (
	maxHostLen   = 253
	maxAddrLen   = 300
	maxUDPData   = 65535
	maxVaultLen  = 2048
	maxOwnerLen  = 64
	maxPubKeyLen = 64
	maxCumLen    = 16
	maxSigLen    = 64
	maxCodeLen   = 64
)

// TCPConnect is the body of TCP_CONNECT.
type TCPConnect struct {
	Host  string
	Port  uint32
	Flags uint32
}

// Result is the body of RESULT.
type Result struct {
	Code uint32
}

// UDPOpen is the body of UDP_OPEN.
type UDPOpen struct {
	Req   uint64
	Flags uint32
}

// UDPOK is the body of UDP_OK. Session 0 means replies travel as UDP_PKT frames on the stream;
// a non-zero session is the id of the EXIT_UDP datagrams of this tunnel.
type UDPOK struct {
	Req     uint64
	Session uint32
	Code    uint32
}

// UDPPkt is the body of UDP_PKT (stream fallback for UDP).
type UDPPkt struct {
	Addr string
	Data []byte
}

// Account is the body of ACCOUNT (services.md §7).
type Account struct {
	Price      uint64
	Credit     uint64
	FreeLeft   uint64
	PayAddress string
	PayTag     string
	MinPay     uint64
	Assets     []uint32
	Vault      []byte
}

// Voucher is the body of VOUCHER.
type Voucher struct {
	Req        uint64
	Owner      []byte
	PubKey     []byte
	Cumulative []byte
	Sig        []byte
	CodeHash   []byte
}

// VoucherResult is the body of VOUCHER_RESULT.
type VoucherResult struct {
	Req        uint64
	OK         bool
	Error      uint32
	Credit     uint64
	Cumulative []byte
}

// Append encodes m canonically: ascending field numbers, default values omitted.
func (m TCPConnect) Append(b []byte) []byte {
	b = pbString(b, 1, m.Host)
	b = pbUint(b, 2, uint64(m.Port))
	return pbUint(b, 3, uint64(m.Flags))
}

// ParseTCPConnect decodes TCP_CONNECT.
func ParseTCPConnect(p []byte) (TCPConnect, error) {
	var m TCPConnect
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			if typ != protowire.BytesType || len(s) > maxHostLen || !utf8.Valid(s) {
				return malformed("tcp_connect.host")
			}
			m.Host = string(s)
		case 2:
			if typ != protowire.VarintType || v == 0 || v > 65535 {
				return malformed("tcp_connect.port")
			}
			m.Port = uint32(v)
		case 3:
			if typ != protowire.VarintType || v > math.MaxUint32 {
				return malformed("tcp_connect.flags")
			}
			m.Flags = uint32(v)
		}
		return nil
	})
	if err != nil {
		return TCPConnect{}, err
	}
	if m.Host == "" || m.Port == 0 {
		return TCPConnect{}, malformed("tcp_connect: host and port are required")
	}
	return m, nil
}

// Append encodes r.
func (r Result) Append(b []byte) []byte { return pbUint(b, 1, uint64(r.Code)) }

// ParseResult decodes RESULT.
func ParseResult(p []byte) (Result, error) {
	var r Result
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, _ []byte) error {
		if num != 1 {
			return nil
		}
		if typ != protowire.VarintType || v > math.MaxUint32 {
			return malformed("result.code")
		}
		r.Code = uint32(v)
		return nil
	})
	return r, err
}

// Append encodes u.
func (u UDPOpen) Append(b []byte) []byte {
	b = pbUint(b, 1, u.Req)
	return pbUint(b, 2, uint64(u.Flags))
}

// ParseUDPOpen decodes UDP_OPEN.
func ParseUDPOpen(p []byte) (UDPOpen, error) {
	var u UDPOpen
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, _ []byte) error {
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return malformed("udp_open.req")
			}
			u.Req = v
		case 2:
			if typ != protowire.VarintType || v > math.MaxUint32 {
				return malformed("udp_open.flags")
			}
			u.Flags = uint32(v)
		}
		return nil
	})
	return u, err
}

// Append encodes u.
func (u UDPOK) Append(b []byte) []byte {
	b = pbUint(b, 1, u.Req)
	b = pbUint(b, 2, uint64(u.Session))
	return pbUint(b, 3, uint64(u.Code))
}

// ParseUDPOK decodes UDP_OK.
func ParseUDPOK(p []byte) (UDPOK, error) {
	var u UDPOK
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, _ []byte) error {
		if typ != protowire.VarintType {
			return malformed("udp_ok field")
		}
		switch num {
		case 1:
			u.Req = v
		case 2:
			if v > math.MaxUint32 {
				return malformed("udp_ok.session")
			}
			u.Session = uint32(v)
		case 3:
			if v > math.MaxUint32 {
				return malformed("udp_ok.code")
			}
			u.Code = uint32(v)
		}
		return nil
	})
	return u, err
}

// Append encodes u.
func (u UDPPkt) Append(b []byte) []byte {
	b = pbString(b, 1, u.Addr)
	return pbBytes(b, 2, u.Data)
}

// ParseUDPPkt decodes UDP_PKT.
func ParseUDPPkt(p []byte) (UDPPkt, error) {
	var u UDPPkt
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, _ uint64, s []byte) error {
		if typ != protowire.BytesType {
			return malformed("udp_pkt field")
		}
		switch num {
		case 1:
			if len(s) > maxAddrLen || !utf8.Valid(s) {
				return malformed("udp_pkt.addr")
			}
			u.Addr = string(s)
		case 2:
			if len(s) > maxUDPData {
				return malformed("udp_pkt.data")
			}
			u.Data = append([]byte(nil), s...)
		}
		return nil
	})
	if err != nil {
		return UDPPkt{}, err
	}
	if u.Addr == "" {
		return UDPPkt{}, malformed("udp_pkt: address is required")
	}
	return u, nil
}

// Append encodes a.
func (a Account) Append(b []byte) []byte {
	b = pbUint(b, 1, a.Price)
	b = pbUint(b, 2, a.Credit)
	b = pbUint(b, 3, a.FreeLeft)
	b = pbString(b, 4, a.PayAddress)
	b = pbString(b, 5, a.PayTag)
	b = pbUint(b, 6, a.MinPay)
	if len(a.Assets) > 0 {
		var packed []byte
		for _, v := range a.Assets {
			packed = protowire.AppendVarint(packed, uint64(v))
		}
		b = pbBytes(b, 7, packed)
	}
	return pbBytes(b, 8, a.Vault)
}

// ParseAccount decodes ACCOUNT.
func ParseAccount(p []byte) (Account, error) {
	var a Account
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1, 2, 3, 6:
			if typ != protowire.VarintType {
				return malformed("account number field")
			}
			switch num {
			case 1:
				a.Price = v
			case 2:
				a.Credit = v
			case 3:
				a.FreeLeft = v
			case 6:
				a.MinPay = v
			}
		case 4, 5:
			if typ != protowire.BytesType || !utf8.Valid(s) {
				return malformed("account string field")
			}
			if num == 4 {
				a.PayAddress = string(s)
			} else {
				a.PayTag = string(s)
			}
		case 7:
			switch typ {
			case protowire.BytesType:
				rest := s
				for len(rest) > 0 {
					x, n := protowire.ConsumeVarint(rest)
					if n < 0 || x > math.MaxUint32 {
						return malformed("account.assets")
					}
					a.Assets = append(a.Assets, uint32(x))
					rest = rest[n:]
				}
			case protowire.VarintType:
				if v > math.MaxUint32 {
					return malformed("account.assets")
				}
				a.Assets = append(a.Assets, uint32(v))
			default:
				return malformed("account.assets")
			}
		case 8:
			if typ != protowire.BytesType || len(s) > maxVaultLen {
				return malformed("account.vault")
			}
			a.Vault = append([]byte(nil), s...)
		}
		return nil
	})
	return a, err
}

// Append encodes v.
func (v Voucher) Append(b []byte) []byte {
	b = pbUint(b, 1, v.Req)
	b = pbBytes(b, 2, v.Owner)
	b = pbBytes(b, 3, v.PubKey)
	b = pbBytes(b, 4, v.Cumulative)
	b = pbBytes(b, 5, v.Sig)
	return pbBytes(b, 6, v.CodeHash)
}

// ParseVoucher decodes VOUCHER.
func ParseVoucher(p []byte) (Voucher, error) {
	var v Voucher
	bounds := map[protowire.Number]int{2: maxOwnerLen, 3: maxPubKeyLen, 4: maxCumLen, 5: maxSigLen, 6: maxCodeLen}
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, x uint64, s []byte) error {
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return malformed("voucher.req")
			}
			v.Req = x
			return nil
		}
		max, ok := bounds[num]
		if !ok {
			return nil
		}
		if typ != protowire.BytesType || len(s) > max {
			return malformed("voucher field")
		}
		cp := append([]byte(nil), s...)
		switch num {
		case 2:
			v.Owner = cp
		case 3:
			v.PubKey = cp
		case 4:
			v.Cumulative = cp
		case 5:
			v.Sig = cp
		case 6:
			v.CodeHash = cp
		}
		return nil
	})
	return v, err
}

// Append encodes v.
func (v VoucherResult) Append(b []byte) []byte {
	b = pbUint(b, 1, v.Req)
	if v.OK {
		b = pbUint(b, 2, 1)
	}
	b = pbUint(b, 3, uint64(v.Error))
	b = pbUint(b, 4, v.Credit)
	return pbBytes(b, 5, v.Cumulative)
}

// ParseVoucherResult decodes VOUCHER_RESULT.
func ParseVoucherResult(p []byte) (VoucherResult, error) {
	var v VoucherResult
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, x uint64, s []byte) error {
		switch num {
		case 1, 3, 4:
			if typ != protowire.VarintType {
				return malformed("voucher_result number field")
			}
			switch num {
			case 1:
				v.Req = x
			case 3:
				if x > math.MaxUint32 {
					return malformed("voucher_result.error")
				}
				v.Error = uint32(x)
			case 4:
				v.Credit = x
			}
		case 2:
			if typ != protowire.VarintType || x > 1 {
				return malformed("voucher_result.ok")
			}
			v.OK = x == 1
		case 5:
			if typ != protowire.BytesType || len(s) > maxCumLen {
				return malformed("voucher_result.cumulative")
			}
			v.Cumulative = append([]byte(nil), s...)
		}
		return nil
	})
	return v, err
}

func malformed(what string) error {
	return fmt.Errorf("%w: %s", frame.ErrMalformed, what)
}

func pbUint(b []byte, num protowire.Number, v uint64) []byte {
	if v == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func pbString(b []byte, num protowire.Number, s string) []byte {
	if s == "" {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func pbBytes(b []byte, num protowire.Number, p []byte) []byte {
	if len(p) == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, p)
}

// pbWalk calls fn for every field of a protobuf message. Unknown fields reach fn too, which
// ignores them by number. Malformed wire data is frame.ErrMalformed.
func pbWalk(p []byte, fn func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error) error {
	for len(p) > 0 {
		num, typ, n := protowire.ConsumeTag(p)
		if n < 0 || num < 1 {
			return malformed("protobuf tag")
		}
		p = p[n:]
		var v uint64
		var s []byte
		switch typ {
		case protowire.VarintType:
			v, n = protowire.ConsumeVarint(p)
		case protowire.BytesType:
			s, n = protowire.ConsumeBytes(p)
		case protowire.Fixed32Type:
			var x uint32
			x, n = protowire.ConsumeFixed32(p)
			v = uint64(x)
		case protowire.Fixed64Type:
			v, n = protowire.ConsumeFixed64(p)
		default:
			return malformed("protobuf wire type")
		}
		if n < 0 {
			return malformed("protobuf value")
		}
		p = p[n:]
		if err := fn(num, typ, v, s); err != nil {
			return err
		}
	}
	return nil
}
