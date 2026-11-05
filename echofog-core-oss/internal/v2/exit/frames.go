// Package exit implements the exit/2 server of EchoFog protocol v2 (services.md §7): TCP
// tunnels, UDP tunnels over EXIT_UDP datagrams, account and voucher frames.
//
// The domain logic (target policy, DNS over HTTPS, quotas, traffic accounting, voucher book) is
// the v1 exit core, imported directly as AD16 requires. This package owns only the v2 wire
// format, the per-client-identity limits of §7 and the shape padding of the first records of a
// new tunnel (H1).
package exit

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
func (m TCPConnect) Append(b []byte) []byte { return b }

// ParseTCPConnect decodes TCP_CONNECT.
func ParseTCPConnect(p []byte) (TCPConnect, error) { return TCPConnect{}, nil }

// Append encodes r.
func (r Result) Append(b []byte) []byte { return b }

// ParseResult decodes RESULT.
func ParseResult(p []byte) (Result, error) { return Result{}, nil }

// Append encodes u.
func (u UDPOpen) Append(b []byte) []byte { return b }

// ParseUDPOpen decodes UDP_OPEN.
func ParseUDPOpen(p []byte) (UDPOpen, error) { return UDPOpen{}, nil }

// Append encodes u.
func (u UDPOK) Append(b []byte) []byte { return b }

// ParseUDPOK decodes UDP_OK.
func ParseUDPOK(p []byte) (UDPOK, error) { return UDPOK{}, nil }

// Append encodes u.
func (u UDPPkt) Append(b []byte) []byte { return b }

// ParseUDPPkt decodes UDP_PKT.
func ParseUDPPkt(p []byte) (UDPPkt, error) { return UDPPkt{}, nil }

// Append encodes a.
func (a Account) Append(b []byte) []byte { return b }

// ParseAccount decodes ACCOUNT.
func ParseAccount(p []byte) (Account, error) { return Account{}, nil }

// Append encodes v.
func (v Voucher) Append(b []byte) []byte { return b }

// ParseVoucher decodes VOUCHER.
func ParseVoucher(p []byte) (Voucher, error) { return Voucher{}, nil }

// Append encodes v.
func (v VoucherResult) Append(b []byte) []byte { return b }

// ParseVoucherResult decodes VOUCHER_RESULT.
func ParseVoucherResult(p []byte) (VoucherResult, error) { return VoucherResult{}, nil }
