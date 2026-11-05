// Package punch is the rendezvous and hole-punching layer of relay/2 (docs/protocol/v2/services.md
// §6.3, §6.4; task T6.2).
package punch

import "time"

// Day is the wire day number: floor(unix / 86400).
func Day(t time.Time) uint32 { return 0 }

// RendezvousID is HMAC-SHA256(S_c, "echofog/relay2/rendezvous" ‖ u32(day)) (services.md §6.3).
func RendezvousID(secret []byte, day uint32) [32]byte { return [32]byte{} }

// IDSet is the rendezvous IDs a node registers: one per contact secret and day.
type IDSet struct {
	Day      uint32
	Today    [][32]byte
	Tomorrow [][32]byte
}

// IDs derives the IDs to register for secrets at now, at most maxIDs in all.
func IDs(secrets [][]byte, now time.Time, maxIDs int) IDSet { return IDSet{} }
