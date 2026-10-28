package network

import (
	"encoding/hex"
	"errors"
	"net/url"
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
	ExpectPeerSNI(addr string, id types.PeerID, sni string)
}

// SplitPeerAddr splits "host:port/<64 hex characters>" into the address and the peer ID.
// An address without ID is returned as it is, with ok false.
func SplitPeerAddr(s string) (addr string, id types.PeerID, ok bool) {
	addr, id, _, ok = SplitPeerAddrSNI(s)
	return
}

// SplitPeerAddrSNI splits "host:port/<64 hex characters>?sni=..." into address, peer ID, and SNI.
// The query is stripped from the address whether or not a peer ID is present: a peer-exchange or
// DHT entry is "host:port?sni=..." (no ID, the ID is in the key next to it), and the caller must
// get a dialable "host:port" back, not the string it passed in.
func SplitPeerAddrSNI(s string) (addr string, id types.PeerID, sni string, ok bool) {
	a := ParseAddrInfo(s)
	return a.Addr, a.ID, a.SNI, a.Named
}

// AddrInfo is what travels with a node's address: the camouflage SNI of a node that mimics a cover
// site, and whether the node runs in the server role (a VPS relay or exit). A client must reach a
// server across a censoring border over REALITY, never over noise, so both facts have to arrive
// with the address through every path (join link, card, peer exchange, DHT), and a server whose
// SNI got lost on the way is told from an ordinary node by Server.
type AddrInfo struct {
	Addr   string       // host:port, or a ws:// URL, with no query and no peer ID
	ID     types.PeerID // valid when Named
	Named  bool         // the string carried a peer ID
	SNI    string
	Server bool
}

// ParseAddrInfo reads "host:port[/<peer id>][?sni=...&srv=1]", also in the echofog://relay/ link
// form. A string with no peer ID gives Named false and the address with its query removed.
func ParseAddrInfo(s string) AddrInfo {
	raw := s
	t := strings.TrimPrefix(strings.TrimSpace(s), RelayLinkPrefix)
	var info AddrInfo
	hadQuery := false
	if qIdx := strings.IndexByte(t, '?'); qIdx >= 0 {
		hadQuery = true
		queryPart := t[qIdx+1:]
		t = t[:qIdx]
		if vals, err := url.ParseQuery(queryPart); err == nil {
			info.SNI = vals.Get("sni")
			info.Server = vals.Get(srvParam) == "1"
		}
	}
	i := strings.LastIndexByte(t, '/')
	if i >= 0 && !strings.HasSuffix(t[:i], ":/") && !strings.HasSuffix(t[:i], ":") {
		if idRaw, err := hex.DecodeString(t[i+1:]); err == nil && len(idRaw) == len(info.ID) {
			copy(info.ID[:], idRaw)
			info.Addr, info.Named = t[:i], true
			return info
		}
	}
	if hadQuery {
		info.Addr = t
	} else {
		info.Addr = raw
	}
	return info
}

// srvParam marks, in an address query, a node that runs in the server role.
const srvParam = "srv"

// AnnotateAddr puts a node's SNI and server flag into the query of a published address, once: an
// address that already carries them is returned as it is.
func AnnotateAddr(addr, sni string, server bool) string {
	if sni != "" && !strings.Contains(addr, "sni=") {
		addr = appendQuery(addr, "sni="+url.QueryEscape(sni))
	}
	if server && !strings.Contains(addr, srvParam+"=") {
		addr = appendQuery(addr, srvParam+"=1")
	}
	return addr
}

func appendQuery(addr, kv string) string {
	if strings.Contains(addr, "?") {
		return addr + "&" + kv
	}
	return addr + "?" + kv
}

// RelayLinkPrefix starts the shareable form of a node address: "echofog://relay/<host:port>/<id>".
const RelayLinkPrefix = "echofog://relay/"

// JoinPeerAddr is the inverse of SplitPeerAddr: the form in which a node is published for others.
func JoinPeerAddr(addr string, id types.PeerID) string { return addr + "/" + id.String() }

// JoinPeerAddrWithSNI appends the node's SNI to the address as a query parameter when present.
func JoinPeerAddrWithSNI(addr string, id types.PeerID, sni string) string {
	return JoinPeerAddrInfo(addr, id, sni, false)
}

// JoinPeerAddrInfo is JoinPeerAddr with the SNI and the server flag: the form in which a camouflage
// or server node publishes itself in a join link.
func JoinPeerAddrInfo(addr string, id types.PeerID, sni string, server bool) string {
	return AnnotateAddr(addr+"/"+id.String(), sni, server)
}

// peerBook remembers which peer ID and SNI an address was announced for.
type peerBook struct {
	mu     sync.Mutex
	m      map[string]types.PeerID
	sni    map[string]string       // addr -> SNI
	byPeer map[types.PeerID]string // peer ID -> SNI
}

const maxBookEntries = 16384

func (b *peerBook) set(addr string, id types.PeerID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil || len(b.m) >= maxBookEntries {
		b.m = make(map[string]types.PeerID)
		b.sni = make(map[string]string)
		b.byPeer = make(map[types.PeerID]string)
	}
	b.m[addr] = id
}

func (b *peerBook) setSNI(addr string, id types.PeerID, sni string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil || len(b.m) >= maxBookEntries {
		b.m = make(map[string]types.PeerID)
		b.sni = make(map[string]string)
		b.byPeer = make(map[types.PeerID]string)
	}
	if id != (types.PeerID{}) {
		b.m[addr] = id
		if sni != "" {
			if b.byPeer == nil {
				b.byPeer = make(map[types.PeerID]string)
			}
			b.byPeer[id] = sni
		}
	}
	if sni != "" {
		if b.sni == nil {
			b.sni = make(map[string]string)
		}
		b.sni[addr] = sni
	}
}

func (b *peerBook) get(addr string) (types.PeerID, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.m[addr]
	return id, ok
}

func (b *peerBook) getSNI(addr string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sni != nil {
		if s, ok := b.sni[addr]; ok && s != "" {
			return s, true
		}
	}
	if id, ok := b.m[addr]; ok && b.byPeer != nil {
		if s, ok2 := b.byPeer[id]; ok2 && s != "" {
			return s, true
		}
	}
	return "", false
}

func (b *peerBook) getSNIByID(id types.PeerID) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byPeer != nil {
		s, ok := b.byPeer[id]
		return s, ok && s != ""
	}
	return "", false
}
