// Package service implements the L3 service layer of EchoFog protocol v2
// (docs/protocol/v2/services.md, versioning.md §3.4): the service registry with each service's
// frame types and maximum payload sizes, per-service version negotiation, dispatch of incoming
// streams with UNSUPPORTED for anything not selected, and the L2 feature list with GREASE.
//
// It contains no service logic: handlers are registered by the packages that implement dht/2,
// chat/2 and the rest.
package service

import (
	"github.com/echofog/echofog-core/internal/v2/frame"
)

// Service IDs (registries.md §3).
const (
	DHT     uint64 = 1
	PEX     uint64 = 2
	Chat    uint64 = 3
	Mailbox uint64 = 4
	Relay   uint64 = 5
	Exit    uint64 = 6
	Files   uint64 = 7
	Call    uint64 = 8
	Names   uint64 = 9  // reserved: crypto usernames, later
	Sync    uint64 = 10 // reserved: group log sync, later

	// ExperimentalMin..ExperimentalMax: service IDs for testing between consenting builds.
	ExperimentalMin uint64 = 0xf000
	ExperimentalMax uint64 = 0xffff
)

// FrameType is one service-local frame type and its normative maximum payload.
type FrameType struct {
	Type uint64
	Name string
	Max  int
	// EarlySafe: the operation may travel in 0-RTT data (registries.md §3, handshake.md §10).
	EarlySafe bool
}

// Version is one version of a service: its frame types.
type Version struct {
	Types []FrameType
}

// Limiter returns the frame limiter of this version: a type without a stated maximum does not
// exist (registries.md §4).
func (v Version) Limiter() frame.Limiter {
	m := make(map[uint64]int, len(v.Types))
	for _, t := range v.Types {
		m[t.Type] = t.Max
	}
	return func(typ uint64) (int, bool) {
		max, ok := m[typ]
		return max, ok
	}
}

// Spec describes a service and the versions this build knows.
type Spec struct {
	ID       uint64
	Name     string
	Versions map[uint64]Version
}

func v2(types ...FrameType) map[uint64]Version { return map[uint64]Version{2: {Types: types}} }

// builtin is the v2 service table of services.md (§2–§9) with the "Max" columns.
var builtin = map[uint64]Spec{
	DHT: {DHT, "dht", v2(
		FrameType{0x40, "FIND_NODE", 64, true}, FrameType{0x41, "NODES", 65536, false},
		FrameType{0x42, "FIND_VALUE", 64, true}, FrameType{0x43, "VALUE", 65536, false},
		FrameType{0x44, "STORE", 24576, false}, FrameType{0x45, "STORED", 32, false},
		FrameType{0x46, "ERROR", 32, false})},
	PEX: {PEX, "pex", v2(
		FrameType{0x40, "GET", 64, true}, FrameType{0x41, "CARDS", 98304, false})},
	Chat: {Chat, "chat", v2(
		FrameType{0x40, "DELIVER", 61504, false}, FrameType{0x41, "DELIVERED", 32, false},
		FrameType{0x42, "BUNDLE_GET", 16, false}, FrameType{0x43, "BUNDLE", 8256, false})},
	Mailbox: {Mailbox, "mailbox", v2(
		FrameType{0x40, "QUEUE_CREATE", 128, false}, FrameType{0x41, "QUEUE_OK", 32, false},
		FrameType{0x42, "DEPOSIT", 65664, false}, FrameType{0x43, "DEPOSITED", 48, false},
		FrameType{0x44, "FETCH", 160, false}, FrameType{0x45, "ITEMS", 1048576, false},
		FrameType{0x46, "ACK", 2048, false}, FrameType{0x47, "FORWARD", 73728, false},
		FrameType{0x48, "ERROR", 32, false})},
	Relay: {Relay, "relay", v2(
		FrameType{0x40, "CONNECT", 256, false}, FrameType{0x41, "INCOMING", 128, false},
		FrameType{0x42, "ACCEPT", 32, false}, FrameType{0x43, "REFUSE", 16, false},
		FrameType{0x44, "CONNECTED", 64, false}, FrameType{0x45, "REFUSED", 64, false},
		FrameType{0x46, "REGISTER", 8320, false}, FrameType{0x47, "REGISTERED", 32, false},
		FrameType{0x48, "PEER_ADDR", 32, false})},
	Exit: {Exit, "exit", v2(
		FrameType{0x40, "TCP_CONNECT", 288, false}, FrameType{0x41, "RESULT", 8, false},
		FrameType{0x42, "DATA", 16384, false}, FrameType{0x43, "UDP_OPEN", 16, false},
		FrameType{0x44, "UDP_OK", 32, false}, FrameType{0x45, "UDP_PKT", 65800, false},
		FrameType{0x46, "ACCOUNT_GET", 0, false}, FrameType{0x47, "ACCOUNT", 2048, false},
		FrameType{0x48, "VOUCHER", 512, false}, FrameType{0x49, "VOUCHER_RESULT", 64, false})},
	Files: {Files, "files", v2(
		FrameType{0x40, "WANT", 4096, false}, FrameType{0x41, "CHUNK", 67584, false},
		FrameType{0x42, "CANCEL", 32, false}, FrameType{0x43, "DONE", 32, false})},
	Call: {Call, "call", v2(
		FrameType{0x40, "SIGNAL", 65536, false})},
}

// Builtin returns the v2 definition of a service. Reserved IDs (0, names, sync) have none.
func Builtin(id uint64) (Spec, bool) {
	s, ok := builtin[id]
	return s, ok
}

// EarlySafe reports whether a service operation may be carried in 0-RTT data.
func EarlySafe(service, version, typ uint64) bool {
	sp, ok := builtin[service]
	if !ok {
		return false
	}
	for _, t := range sp.Versions[version].Types {
		if t.Type == typ {
			return t.EarlySafe
		}
	}
	return false
}

// IsGreaseService reports whether a service ID is a GREASE value (0xXAXA, registries.md §5).
func IsGreaseService(id uint64) bool { return id <= 0xffff && frame.IsGrease16(uint16(id)) }
