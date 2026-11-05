package frame

// Session-level frame types (registries.md §1).
const (
	TypePadding      uint64 = 0x01
	TypeHsHello      uint64 = 0x02
	TypeHsMsg        uint64 = 0x03
	TypeHsRetry      uint64 = 0x04
	TypeReject       uint64 = 0x05
	TypePing         uint64 = 0x06
	TypePong         uint64 = 0x07
	TypeGoAway       uint64 = 0x08
	TypeClose        uint64 = 0x09
	TypeOpen         uint64 = 0x0a
	TypeUnsupported  uint64 = 0x0b
	TypeObservedAddr uint64 = 0x0c
	TypeCardUpdate   uint64 = 0x0d
	TypePathBind     uint64 = 0x10
	TypePathBindOK   uint64 = 0x11
	TypePathAbandon  uint64 = 0x12
	TypePunchOffer   uint64 = 0x13
	TypePunchSync    uint64 = 0x14
	TypeMuxStream    uint64 = 0x18
	TypeMuxReset     uint64 = 0x19
	TypeMuxWindow    uint64 = 0x1a
	TypeMuxMaxData   uint64 = 0x1b
	TypeMuxDatagram  uint64 = 0x1c
	// TypeGrease is never assigned: a receiver always treats it as unknown (and, since senders set
	// IGNORABLE on it, skips it). It is not in the receive registry on purpose.
	TypeGrease uint64 = 0x3e
	// GreaseMaxPayload is the largest GREASE payload a sender uses (registries.md §1).
	GreaseMaxPayload = 256

	// ServiceLocalMin is the first service-local type; ServiceLocalMax the last (registries.md §4).
	ServiceLocalMin uint64 = 0x40
	ServiceLocalMax uint64 = 0x3fff
)

// Where a session-level type is valid (registries.md §1 "Where" column).
type Where uint8

const (
	WhereAny       Where = 1 << iota // A: any stream
	WhereControl                     // S: stream 0 after the handshake
	WherePreHS                       // H: stream 0 before the handshake completes
	WhereConn                        // B: byte-stream carrier, connection level
	WhereFirstOpen                   // OPEN: first frame of a non-control stream
)

// Feature IDs that gate session-level frames (registries.md §5). Defined here so the frame
// registry can say which frame a feature introduces; the service package owns the feature list.
const (
	FeaturePathMigration uint16 = 0x0001
	FeatureIdlePadding   uint16 = 0x0003
	FeatureObservedAddr  uint16 = 0x0004
	FeatureCardPush      uint16 = 0x0006
)

// TypeInfo describes a known session-level frame type.
type TypeInfo struct {
	Name  string
	Max   int   // maximum payload length
	Where Where // where the type is valid
	// Feature, when non-zero, is the feature that introduces the frame: it is valid only after
	// that feature was negotiated (wire.md §6.2, versioning.md §3.3).
	Feature uint16
}

var sessionTypes = map[uint64]TypeInfo{
	TypePadding:      {"PADDING", 16384, WhereAny | WhereControl | WherePreHS | WhereConn, 0},
	TypeHsHello:      {"HS_HELLO", 64, WherePreHS, 0},
	TypeHsMsg:        {"HS_MSG", 16384, WherePreHS, 0},
	TypeHsRetry:      {"HS_RETRY", 32, WherePreHS, 0},
	TypeReject:       {"REJECT", 512, WherePreHS | WhereControl, 0},
	TypePing:         {"PING", 8, WhereControl, 0},
	TypePong:         {"PONG", 8, WhereControl, 0},
	TypeGoAway:       {"GOAWAY", 256, WhereControl, 0},
	TypeClose:        {"CLOSE", 210, WhereAny, 0},
	TypeOpen:         {"OPEN", 24, WhereFirstOpen, 0},
	TypeUnsupported:  {"UNSUPPORTED", 24, WhereAny, 0},
	TypeObservedAddr: {"OBSERVED_ADDR", 19, WhereControl, FeatureObservedAddr},
	TypeCardUpdate:   {"CARD_UPDATE", 4096, WhereControl, FeatureCardPush},
	TypePathBind:     {"PATH_BIND", 56, WherePreHS, FeaturePathMigration},
	TypePathBindOK:   {"PATH_BIND_OK", 8, WherePreHS, FeaturePathMigration},
	TypePathAbandon:  {"PATH_ABANDON", 16, WhereControl, 0},
	TypePunchOffer:   {"PUNCH_OFFER", 256, WhereControl, FeaturePathMigration},
	TypePunchSync:    {"PUNCH_SYNC", 8, WhereControl, FeaturePathMigration},
	TypeMuxStream:    {"MUX_STREAM", 16400, WhereConn, 0},
	TypeMuxReset:     {"MUX_RESET", 16, WhereConn, 0},
	TypeMuxWindow:    {"MUX_WINDOW", 16, WhereConn, 0},
	TypeMuxMaxData:   {"MUX_MAX_DATA", 8, WhereConn, 0},
	TypeMuxDatagram:  {"MUX_DATAGRAM", 1452, WhereConn, 0},
}

// SessionType returns the registry entry of a known session-level type. Reserved (0x00, 0x3f),
// unassigned and GREASE values are not known.
func SessionType(t uint64) (TypeInfo, bool) {
	i, ok := sessionTypes[t]
	return i, ok
}

// TypeName returns a printable name for a type.
func TypeName(t uint64) string {
	if i, ok := sessionTypes[t]; ok {
		return i.Name
	}
	if t == TypeGrease {
		return "GREASE"
	}
	return "unknown"
}

// IsGrease16 reports whether v is one of the 15 GREASE values 0xXAXA, X in 0..e (registries.md
// §5, D18). The same values are reserved as feature IDs, service IDs and L1 suite IDs.
func IsGrease16(v uint16) bool {
	return v&0x0f0f == 0x0a0a && v>>12 == (v>>4)&0x0f && v>>12 <= 0x0e
}

// CLOSE, GOAWAY and PATH_ABANDON codes (registries.md §10.2).
const (
	CodeNoError            uint64 = 0
	CodeProtocolViolation  uint64 = 1
	CodeUnknownFrame       uint64 = 2
	CodeFrameTooLarge      uint64 = 3
	CodeServiceUnsupported uint64 = 4
	CodeRefused            uint64 = 5
	CodeTimeout            uint64 = 6
	CodeOverloaded         uint64 = 7
	CodeGoingAway          uint64 = 8
	CodePathFailed         uint64 = 9
	CodeInternal           uint64 = 10
	// CodeServiceMin is the first service-defined code.
	CodeServiceMin uint64 = 0x100
)

// Datagram types (registries.md §2).
const (
	DgramPadding    uint64 = 0x01
	DgramSealed     uint64 = 0x02
	DgramCallAudio  uint64 = 0x10
	DgramCallVideo  uint64 = 0x11
	DgramCallSignal uint64 = 0x12
	DgramCallAuth   uint64 = 0x13
	DgramExitUDP    uint64 = 0x20
	DgramRelay      uint64 = 0x30
)

var datagramTypes = map[uint64]string{
	DgramPadding: "PADDING", DgramSealed: "SEALED", DgramCallAudio: "CALL_AUDIO",
	DgramCallVideo: "CALL_VIDEO", DgramCallSignal: "CALL_SIGNAL", DgramCallAuth: "CALL_AUTH",
	DgramExitUDP: "EXIT_UDP", DgramRelay: "RELAY_DGRAM",
}

// DatagramKnown reports whether t is a registered datagram type. Values in the experimental
// service range (0xf000–0xffff, registries.md §3) are allowed between consenting builds, as the
// registry preamble permits for experimental ranges; the lab CLI's echo service uses one.
func DatagramKnown(t uint64) bool {
	if _, ok := datagramTypes[t]; ok {
		return true
	}
	return t >= 0xf000 && t <= 0xffff
}
