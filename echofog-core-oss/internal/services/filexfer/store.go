package filexfer

import (
	"encoding/json"
	"errors"

	"github.com/echofog/echofog-core/internal/storage"
)

// Keys under "fx:" hold the transfers (names, paths and keys: storage.SealedPrefixes encrypts them).
var (
	recPrefix  = []byte("fx:t:")
	bitsPrefix = []byte("fx:b:")
)

// Direction says which side of a transfer this node is.
type Direction string

const (
	Outgoing Direction = "out"
	Incoming Direction = "in"
)

// State is where a transfer stands.
type State string

const (
	StateOffered   State = "offered"   // waiting for the receiver to accept
	StateActive    State = "active"    // accepted: chunks are moving, or will as soon as the peer is reachable
	StateDone      State = "done"      // all of it arrived and was checked
	StateDeclined  State = "declined"  // the receiver did not want it
	StateCancelled State = "cancelled" // one side stopped it
	StateFailed    State = "failed"    // it cannot go on (the sender lost the file, the disk failed)
)

// Final tells whether nothing more happens to a transfer in this state.
func (s State) Final() bool {
	return s == StateDone || s == StateDeclined || s == StateCancelled || s == StateFailed
}

// Record is what is kept of a transfer.
type Record struct {
	ID      string    `json:"id"`
	Peer    string    `json:"peer"`            // the other side; empty for a file offered to a group
	Group   string    `json:"group,omitempty"` // set when the file was offered in a group chat
	Dir     Direction `json:"dir"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mime    string    `json:"mime,omitempty"`
	Root    string    `json:"root"`
	Key     string    `json:"key"`
	Path    string    `json:"path,omitempty"`    // outgoing: the source; incoming: the finished file
	Part    string    `json:"part,omitempty"`    // incoming: where the chunks are written until it is done
	ModTime int64     `json:"modTime,omitempty"` // outgoing: the source's modification time when offered
	State   State     `json:"state"`
	Error   string    `json:"error,omitempty"`
	// outgoing to a group: the members it was offered to, and those who got all of it
	Members   []string `json:"members,omitempty"`
	Delivered []string `json:"delivered,omitempty"`
	Created   int64    `json:"created"`
	Updated   int64    `json:"updated"`
}

func saveRecord(st storage.Storage, r *Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return st.Set(append(append([]byte(nil), recPrefix...), r.ID...), b)
}

func loadRecords(st storage.Storage) ([]*Record, error) {
	entries, err := st.FindValues(recPrefix, 0)
	if err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(entries))
	for _, e := range entries {
		var r Record
		if json.Unmarshal(e.Value, &r) == nil && r.ID != "" {
			out = append(out, &r)
		}
	}
	return out, nil
}

func deleteRecord(st storage.Storage, id string) {
	_ = st.Delete(append(append([]byte(nil), recPrefix...), id...))
	_ = st.Delete(append(append([]byte(nil), bitsPrefix...), id...))
}

func saveBits(st storage.Storage, id string, bits []byte) error {
	return st.Set(append(append([]byte(nil), bitsPrefix...), id...), bits)
}

func loadBits(st storage.Storage, id string, n int) []byte {
	b, err := st.Get(append(append([]byte(nil), bitsPrefix...), id...))
	if err != nil || len(b) != (n+7)/8 {
		return make([]byte, (n+7)/8)
	}
	return b
}

var errNotFound = errors.New("no such transfer")
