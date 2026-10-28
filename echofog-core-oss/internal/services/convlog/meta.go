package convlog

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Conversation kinds.
const (
	KindDM    = "dm"
	KindGroup = "group"
)

// Conv tells the log which conversation an event belongs to and who takes part in it.
type Conv struct {
	ID    ConvID
	Kind  string       // KindDM or KindGroup
	Peer  types.PeerID // KindDM: the other participant
	Group string       // KindGroup: the group id
}

// DM is the direct conversation between self and peer.
func DM(self, peer types.PeerID) Conv {
	return Conv{ID: DMConv(self, peer), Kind: KindDM, Peer: peer}
}

// Group is the conversation of a group.
func Group(groupID string) Conv {
	return Conv{ID: GroupConv(groupID), Kind: KindGroup, Group: groupID}
}

// Meta is the state of one conversation: what the log holds and where the reader is.
type Meta struct {
	Conv  string `json:"conv"`
	Kind  string `json:"kind"`
	Peer  string `json:"peer,omitempty"`  // KindDM: the other peer id hex
	Group string `json:"group,omitempty"` // KindGroup

	// VV is the version vector: per author (peer id hex), the highest seq held with no gap below.
	VV map[string]uint64 `json:"vv,omitempty"`
	// Max is the highest seq held per author, gaps or not.
	Max map[string]uint64 `json:"max,omitempty"`
	// Forks: authors seen publishing two different events with one seq, and that seq.
	Forks map[string]uint64 `json:"forks,omitempty"`

	OwnSeq     uint64 `json:"own_seq,omitempty"`
	MaxLamport uint64 `json:"max_lamport,omitempty"`

	LastID      string `json:"last_id,omitempty"` // the newest top-level message
	LastLamport uint64 `json:"last_lamport,omitempty"`
	LastTS      int64  `json:"last_ts,omitempty"` // ms

	ReadLamport uint64 `json:"read_lamport,omitempty"` // everything up to here was read
	Unread      int    `json:"unread,omitempty"`

	// Rebuild: a membership change arrived that sits before events already applied; the views are
	// computed again from the stored events (see Log.rebuild).
	Rebuild bool `json:"rebuild,omitempty"`
	// HeldLamport is the highest clock of an event kept without effects (not allowed, yet): a
	// membership event at or below it may change what that event was allowed to do.
	HeldLamport uint64 `json:"held_lamport,omitempty"`

	// Chans: a group's channels other than the main one, where each ends and where its reader is.
	Chans map[string]*ChanStat `json:"chans,omitempty"`
}

func newMeta(c Conv) *Meta {
	m := &Meta{Conv: c.ID.String(), Kind: c.Kind, VV: map[string]uint64{}, Max: map[string]uint64{}}
	switch c.Kind {
	case KindDM:
		m.Peer = peerHex(c.Peer)
	case KindGroup:
		m.Group = c.Group
	}
	return m
}

func (m *Meta) init() {
	if m.VV == nil {
		m.VV = map[string]uint64{}
	}
	if m.Max == nil {
		m.Max = map[string]uint64{}
	}
}

// Digest is a short fingerprint of the version vector: two replicas with the same one hold the
// same contiguous history.
func (m *Meta) Digest() []byte {
	authors := make([]string, 0, len(m.VV))
	for a, n := range m.VV {
		if n > 0 {
			authors = append(authors, a)
		}
	}
	sort.Strings(authors)
	h := sha256.New()
	for _, a := range authors {
		h.Write([]byte(a))
		h.Write([]byte{':'})
		h.Write([]byte(strconv.FormatUint(m.VV[a], 10)))
		h.Write([]byte{';'})
	}
	return h.Sum(nil)[:8]
}

func peerHex(id types.PeerID) string { return hex.EncodeToString(id[:]) }
