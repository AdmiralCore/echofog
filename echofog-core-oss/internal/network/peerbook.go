package network

import (
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// ErrPeerUnknown: obfuscation is on, so a dial needs the ID of the node it goes to (its keys derive
// from it), and nobody told the transport whose address this is.
var ErrPeerUnknown = errors.New("network: the peer ID of this address is unknown; use the form host:port/peerid")

// ErrPeerMismatch: the node that answered is not the one the address was announced for.
var ErrPeerMismatch = errors.New("network: the node at this address has another peer ID than expected")

// PeerMismatchError is ErrPeerMismatch with the peer ID that did answer, so the caller can tell a
// stale address of someone else from its own address announced under an old ID.
type PeerMismatchError struct{ Got types.PeerID }

func (e *PeerMismatchError) Error() string        { return ErrPeerMismatch.Error() }
func (e *PeerMismatchError) Is(target error) bool { return target == ErrPeerMismatch }

// PeerExpecter is implemented by transports that need to know whose address they dial.
type PeerExpecter interface {
	ExpectPeer(addr string, id types.PeerID)
}

// SplitPeerAddr splits "host:port/<64 hex characters>" into the address and the peer ID.
// An address without ID is returned as it is, with ok false.
func SplitPeerAddr(s string) (addr string, id types.PeerID, ok bool) {
	// the link the app shows for sharing ("echofog://relay/host:port/<id>") names the same node;
	// people paste it into a node's bootstrap setting as it is
	t := strings.TrimPrefix(strings.TrimSpace(s), RelayLinkPrefix)
	i := strings.LastIndexByte(t, '/')
	if i < 0 {
		return s, id, false
	}
	raw, err := hex.DecodeString(t[i+1:])
	if err != nil || len(raw) != len(id) {
		return s, id, false
	}
	copy(id[:], raw)
	return t[:i], id, true
}

// RelayLinkPrefix starts the shareable form of a node address: "echofog://relay/<host:port>/<id>".
const RelayLinkPrefix = "echofog://relay/"

// JoinPeerAddr is the inverse of SplitPeerAddr: the form in which a node is published for others.
func JoinPeerAddr(addr string, id types.PeerID) string { return addr + "/" + id.String() }

// peerBook remembers which peer ID an address was announced for.
type peerBook struct {
	mu sync.Mutex
	m  map[string]types.PeerID
}

const maxBookEntries = 16384

func (b *peerBook) set(addr string, id types.PeerID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil || len(b.m) >= maxBookEntries {
		b.m = make(map[string]types.PeerID)
	}
	b.m[addr] = id
}

func (b *peerBook) get(addr string) (types.PeerID, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.m[addr]
	return id, ok
}
