package handshake

import "github.com/echofog/echofog-core/internal/v2/noise"

// L1 suite IDs (registries.md §6.2).
const (
	SuiteXK uint16 = 0x0001
	SuiteXX uint16 = 0x0002
)

// Suite is an L1 handshake suite.
type Suite struct {
	ID      uint16
	Name    string
	Pattern *noise.Pattern
}

var suites = map[uint16]Suite{
	SuiteXK: {SuiteXK, noise.NameXKhfs, noise.XKhfs},
	SuiteXX: {SuiteXX, noise.NameXXhfs, noise.XXhfs},
}

// SuiteByID returns a known suite.
func SuiteByID(id uint16) (Suite, bool) {
	s, ok := suites[id]
	return s, ok
}

// DefaultSuites is the v2 preference order: XK before XX (registries.md §6.2).
var DefaultSuites = []uint16{SuiteXK, SuiteXX}

// IsGrease reports whether v is a GREASE value 0xXAXA with X in 0..e (registries.md §5, D18).
func IsGrease(v uint16) bool {
	hi, lo := byte(v>>8), byte(v)
	return hi == lo && lo&0x0f == 0x0a && lo>>4 <= 0x0e
}

// IsGrease32 is IsGrease for the uint32 fields (features, service IDs).
func IsGrease32(v uint32) bool { return v <= 0xffff && IsGrease(uint16(v)) }

// Session-level frame types used by the handshake on stream 0 (registries.md §1).
const (
	FrameHello  = 0x02
	FrameMsg    = 0x03
	FrameRetry  = 0x04
	FrameReject = 0x05
)

// Maximum payload sizes (registries.md §1, handshake.md §11).
const (
	MaxHelloLen  = 64
	MaxRetryLen  = 32
	MaxRejectLen = 512
	MaxCardLen   = 4096
	MaxEarlyLen  = 1200
	MaxEarlyResp = 8192
)

// REJECT reasons (registries.md §10.1).
const (
	ReasonVersion         = 1
	ReasonSuite           = 2
	ReasonFeatureRequired = 3
	ReasonOverloaded      = 4
	ReasonPolicy          = 5
	ReasonBanned          = 6
	ReasonCardInvalid     = 7
	ReasonClockSkew       = 8
	ReasonRateLimited     = 9
	ReasonInternal        = 10
)

// Feature IDs used by the handshake (registries.md §5).
const (
	FeatureObservedAddr = 0x0004
	FeatureEarlyData    = 0x0005
)

// ProtoVer is v2.0 (versioning.md §1).
const ProtoVer = 200
