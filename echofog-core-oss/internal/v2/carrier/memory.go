package carrier

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// DefaultMemoryTTL is how long the working suite is remembered per network and address. It
// matches the v1 fallback-transport memory (30 minutes), so a censored network does not bias
// dials after the device moves back to an open one.
const DefaultMemoryTTL = 30 * time.Minute

const memoryPrefix = "v2/carrier/working/"

// KV is the small persistent key-value view the carrier memory needs. T4.3 adapts the node
// store to it; the carrier package does not depend on the store.
type KV interface {
	// Get returns the value for key and ok=false when the key is absent.
	Get(ctx context.Context, key []byte) (value []byte, ok bool, err error)
	// Put stores value under key.
	Put(ctx context.Context, key, value []byte) error
}

// MemoryConfig configures Memory.
type MemoryConfig struct {
	// KV is the persistent store (T4.3).
	KV KV
	// LocalAddrs returns the device's current non-loopback addresses. On mobile it is the
	// local_addrs callback; net.Interfaces must not be called on the dial path.
	LocalAddrs func() []netip.Addr
	// Now is the injectable clock; nil means time.Now.
	Now func() time.Time
	// TTL is the entry lifetime; zero means DefaultMemoryTTL.
	TTL time.Duration
}

// Memory remembers the suite that last worked for a (network fingerprint, address) pair
// (wire.md §1 rule 5). It is best effort: a missing store, an empty fingerprint or a KV error
// never prevents a dial.
type Memory struct {
	kv    KV
	addrs func() []netip.Addr
	now   func() time.Time
	ttl   time.Duration
}

// NewMemory builds the carrier memory. A nil KV makes it a no-op memory.
func NewMemory(cfg MemoryConfig) *Memory {
	m := &Memory{kv: cfg.KV, addrs: cfg.LocalAddrs, now: cfg.Now, ttl: cfg.TTL}
	if m.now == nil {
		m.now = time.Now
	}
	if m.ttl <= 0 {
		m.ttl = DefaultMemoryTTL
	}
	return m
}

// Lookup returns the suite remembered for addr on the current network.
func (m *Memory) Lookup(ctx context.Context, addr string) (uint32, bool, error) {
	key, ok := m.key(addr)
	if !ok {
		return 0, false, nil
	}
	value, found, err := m.kv.Get(ctx, key)
	if err != nil {
		return 0, false, err
	}
	if !found || len(value) != 12 {
		return 0, false, nil
	}
	suite := binary.BigEndian.Uint32(value[8:])
	at := time.Unix(0, int64(binary.BigEndian.Uint64(value[:8])))
	if m.now().Sub(at) > m.ttl {
		return 0, false, nil
	}
	return suite, true, nil
}

// Remember records that suite worked for addr on the current network.
func (m *Memory) Remember(ctx context.Context, addr string, suite uint32) error {
	key, ok := m.key(addr)
	if !ok {
		return nil
	}
	value := make([]byte, 12)
	binary.BigEndian.PutUint64(value[:8], uint64(m.now().UnixNano()))
	binary.BigEndian.PutUint32(value[8:], suite)
	return m.kv.Put(ctx, key, value)
}

// key builds the storage key for the current network fingerprint and addr.
func (m *Memory) key(addr string) ([]byte, bool) {
	if m == nil || m.kv == nil || addr == "" || m.addrs == nil {
		return nil, false
	}
	fp := networkFingerprint(m.addrs())
	if fp == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(addr))
	return []byte(memoryPrefix + fp + "/" + hex.EncodeToString(sum[:16])), true
}

// prefer moves the attempt of the remembered suite to the front, if one exists. KV errors are
// ignored: memory is an optimisation, not a requirement.
func (m *Memory) prefer(ctx context.Context, atts []Attempt) []Attempt {
	if m == nil || m.kv == nil {
		return atts
	}
	for _, a := range atts {
		if a.Addr == nil {
			continue
		}
		suite, ok, err := m.Lookup(ctx, AddressKey(a.Addr))
		if err != nil || !ok {
			continue
		}
		for i, b := range atts {
			if b.Suite != suite {
				continue
			}
			if i == 0 {
				return atts
			}
			out := make([]Attempt, 0, len(atts))
			out = append(out, b)
			out = append(out, atts[:i]...)
			out = append(out, atts[i+1:]...)
			return out
		}
	}
	return atts
}

// networkFingerprint hashes the sorted set of usable local addresses. An empty result means the
// network is not fingerprinted yet and nothing may be remembered.
func networkFingerprint(addrs []netip.Addr) string {
	parts := make([]string, 0, len(addrs))
	seen := make(map[netip.Addr]struct{}, len(addrs))
	for _, a := range addrs {
		if !a.IsValid() || a.IsLoopback() || a.IsUnspecified() {
			continue
		}
		a = a.Unmap()
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		parts = append(parts, a.String())
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:16])
}

// AddressKey is the address part of the memory key: host:port for direct addresses and the URL
// host with the HTTPS default port for ws2. DNS names are lowercased. An unusable address gives
// an empty key and is never remembered.
func AddressKey(a *pb.Addr) string {
	if a == nil {
		return ""
	}
	if a.GetSuite() == objects.SuiteWS2 {
		u, err := url.Parse(a.GetUrl())
		if err != nil {
			return ""
		}
		host := strings.ToLower(u.Hostname())
		if host == "" {
			return ""
		}
		port := u.Port()
		if port == "" {
			port = "443"
		}
		return net.JoinHostPort(host, port)
	}
	host := strings.ToLower(a.GetHost())
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(int(a.GetPort())))
}
