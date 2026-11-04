package exit_gateway

// This package keeps the v1 API surface: everything that does not depend on the swarm lives in
// exit_gateway/core, and the names below re-export it so callers and tests keep compiling
// unchanged. The swarm wiring itself stays in this package (swarm.go, udp_datagram.go).

import (
	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
)

// Types.
type (
	AccountInfo     = core.AccountInfo
	ClaimPolicy     = core.ClaimPolicy
	ExitService     = core.ExitService
	ExitSplicer     = core.ExitSplicer
	FakeDNS         = core.FakeDNS
	FramedStream    = core.FramedStream
	IncomingPayment = core.IncomingPayment
	Ledger          = core.Ledger
	Limits          = core.Limits
	ManagedSession  = core.ManagedSession
	P2PDialer       = core.P2PDialer
	PaymentSource   = core.PaymentSource
	PeerSessionPool = core.PeerSessionPool
	Policy          = core.Policy
	Refusal         = core.Refusal
	SOCKS5Server    = core.SOCKS5Server
	StreamDialer    = core.StreamDialer
	StreamHealth    = core.StreamHealth
	StreamOpener    = core.StreamOpener
	UDPDialer       = core.UDPDialer
	UDPSession      = core.UDPSession
	VaultChain      = core.VaultChain
	VaultTerms      = core.VaultTerms
	VerdictNotifier = core.VerdictNotifier
	VoucherBook     = core.VoucherBook
	VoucherOffer    = core.VoucherOffer
	VoucherReply    = core.VoucherReply
)

// Functions.
var (
	DefaultClaimPolicy           = core.DefaultClaimPolicy
	DefaultLimits                = core.DefaultLimits
	ExitRefusal                  = core.ExitRefusal
	KeyedPayTag                  = core.KeyedPayTag
	LimitsFromConfig             = core.LimitsFromConfig
	NewExitService               = core.NewExitService
	NewExitSplicer               = core.NewExitSplicer
	NewLedger                    = core.NewLedger
	NewP2PDialer                 = core.NewP2PDialer
	NewPeerSessionPoolWithDialer = core.NewPeerSessionPoolWithDialer
	NewSOCKS5Server              = core.NewSOCKS5Server
	NewVoucherBook               = core.NewVoucherBook
	PayTag                       = core.PayTag
	SetPadSeed                   = core.SetPadSeed
)

// Variables.
var (
	DefaultExitDNS     = core.DefaultExitDNS
	DefaultFakeDNSAddr = core.DefaultFakeDNSAddr
	PayAssets          = core.PayAssets
)

// Constants.
const (
	ExitDNSSystem         = core.ExitDNSSystem
	MinPayMicro           = core.MinPayMicro
	OverdraftBytes        = core.OverdraftBytes
	UDPHelloDatagrams     = core.UDPHelloDatagrams
	VoucherOverdraftBytes = core.VoucherOverdraftBytes

	RefuseDisabled  = core.RefuseDisabled
	RefusePayment   = core.RefusePayment
	RefuseTarget    = core.RefuseTarget
	RefuseDial      = core.RefuseDial
	RefuseBusy      = core.RefuseBusy
	RefuseRate      = core.RefuseRate
	RefuseFreeLimit = core.RefuseFreeLimit
)

// Errors.
var (
	ErrBadDatagram         = core.ErrBadDatagram
	ErrBlockedAddress      = core.ErrBlockedAddress
	ErrExitBusy            = core.ErrExitBusy
	ErrExitDenied          = core.ErrExitDenied
	ErrExitDialFailed      = core.ErrExitDialFailed
	ErrExitDisabled        = core.ErrExitDisabled
	ErrExitProtocol        = core.ErrExitProtocol
	ErrInvalidTarget       = core.ErrInvalidTarget
	ErrNoStreamOpener      = core.ErrNoStreamOpener
	ErrPaymentRequired     = core.ErrPaymentRequired
	ErrSocksAuthFailed     = core.ErrSocksAuthFailed
	ErrSocksAddressNotSupp = core.ErrSocksAddressNotSupp
	ErrSocksCommandNotSupp = core.ErrSocksCommandNotSupp
	ErrVaultUnusable       = core.ErrVaultUnusable
	ErrVoucherRejected     = core.ErrVoucherRejected

	errNoStreamOpener = core.ErrNoStreamOpener
)
