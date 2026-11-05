// Package punch is the rendezvous and hole-punching layer of relay/2 (docs/protocol/v2/services.md
// §6.3, §6.4; task T6.2).
package punch

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"time"
)

const labelRendezvous = "echofog/relay2/rendezvous"

// Day is the wire day number: floor(unix / 86400).
func Day(t time.Time) uint32 { return uint32(t.Unix() / 86400) }

// RendezvousID is HMAC-SHA256(S_c, "echofog/relay2/rendezvous" ‖ u32(day)) (services.md §6.3).
func RendezvousID(secret []byte, day uint32) [32]byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(labelRendezvous))
	m.Write(binary.BigEndian.AppendUint32(nil, day))
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

// IDSet is the rendezvous IDs a node registers: one per contact secret and day.
type IDSet struct {
	Day      uint32
	Today    [][32]byte
	Tomorrow [][32]byte
}

// IDs derives the IDs to register for secrets at now, at most maxIDs in all. Every secret takes
// two slots (the current and the next day); a secret that does not fit whole is dropped, and a
// repeated secret counts once.
func IDs(secrets [][]byte, now time.Time, maxIDs int) IDSet {
	day := Day(now)
	set := IDSet{Day: day}
	seen := make(map[string]struct{}, len(secrets))
	for _, s := range secrets {
		if len(set.Today)+len(set.Tomorrow)+2 > maxIDs {
			break
		}
		if _, dup := seen[string(s)]; dup {
			continue
		}
		seen[string(s)] = struct{}{}
		set.Today = append(set.Today, RendezvousID(s, day))
		set.Tomorrow = append(set.Tomorrow, RendezvousID(s, day+1))
	}
	return set
}
