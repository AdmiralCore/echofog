package dpi

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/blake2b"
)

const (
	// SaltLen is the random salt in front of every packet.
	SaltLen = 8
	// TagLen is the integrity tag behind every packet. It lets a receiver that holds several
	// candidate keys tell which one a packet was sealed with, and drops anything else (scanners,
	// corrupted packets) without answering.
	TagLen = 4
	// Overhead is what obfuscation adds to every datagram.
	Overhead = SaltLen + TagLen
	// KeyLen is the size of a node key.
	KeyLen = blake2b.Size256

	maxPeerKeys = 16384

	// heardFor is how long the key a packet from an address opened with is used to answer it.
	// A live QUIC connection sends far more often; after that, a dial to the address uses the key
	// of the node it is meant for again.
	heardFor = 30 * time.Second
)

var (
	ErrBadKeyLen      = errors.New("dpi/obfs: key must be 32 bytes")
	ErrBufferTooSmall = errors.New("dpi/obfs: destination buffer too small")
)

// NodeKey derives the obfuscation key of a node from its peer ID. Anyone who knows a node's ID can
// dial it; nobody else sees anything but noise. secret is an optional extra secret for a private
// network (empty for the public one). No key is shared by the whole network, and none is stored
// in the program.
func NodeKey(secret string, id [32]byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte("echofog/udp-obfs/v2\x00"))
	h.Write([]byte(secret))
	h.Write([]byte{0})
	h.Write(id[:])
	return h.Sum(nil)
}

// NodeObfs masks datagrams so that every packet looks like random bytes:
// [salt][payload XOR keystream(key, salt)][tag]. A node seals what it sends to a peer with the
// peer's key (known from the peer ID it dialed) and what it sends to anybody else with its own
// key; it opens what it receives with the key of the sender's address if it has one, and with
// its own key otherwise. Once a packet from an address has opened, what goes back to that
// address is sealed with the same key: the answer goes to whoever really speaks from there.
type NodeObfs struct {
	own [KeyLen]byte

	mu    sync.RWMutex
	peers map[netip.AddrPort][KeyLen]byte
	byIP  map[netip.Addr][][KeyLen]byte
	// heard is the key the last packet from an address opened with. The expected key of an
	// address can belong to another node: two computers behind one NAT, where the router gave
	// the first the outside port the second announces, so this node expects the second there
	// while the first dials in from it. Answering the first with the second's key made its
	// handshake fail for ever, while the second, from another port, connected fine.
	heard map[netip.AddrPort]heardKey
}

type heardKey struct {
	key [KeyLen]byte
	at  time.Time
}

// NewNodeObfs creates the obfuscator of a node with the given own key (see NodeKey).
func NewNodeObfs(ownKey []byte) (*NodeObfs, error) {
	if len(ownKey) != KeyLen {
		return nil, ErrBadKeyLen
	}
	o := &NodeObfs{peers: make(map[netip.AddrPort][KeyLen]byte), byIP: make(map[netip.Addr][][KeyLen]byte),
		heard: make(map[netip.AddrPort]heardKey)}
	copy(o.own[:], ownKey)
	return o, nil
}

func normalize(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func addrPortOf(a net.Addr) (netip.AddrPort, bool) {
	if v, ok := a.(*net.UDPAddr); ok {
		return normalize(v.AddrPort()), true
	}
	ap, err := netip.ParseAddrPort(a.String())
	return normalize(ap), err == nil
}

// Expect records that the node at addr uses key. Call it before dialing addr.
func (o *NodeObfs) Expect(addr netip.AddrPort, key []byte) error {
	if len(key) != KeyLen {
		return ErrBadKeyLen
	}
	addr = normalize(addr)
	var k [KeyLen]byte
	copy(k[:], key)
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.peers) >= maxPeerKeys {
		o.peers = make(map[netip.AddrPort][KeyLen]byte)
		o.byIP = make(map[netip.Addr][][KeyLen]byte)
	}
	o.peers[addr] = k
	list := o.byIP[addr.Addr()]
	for _, have := range list {
		if have == k {
			return nil
		}
	}
	if len(list) < 8 {
		o.byIP[addr.Addr()] = append(list, k)
	}
	return nil
}

func (o *NodeObfs) sendKey(addr net.Addr) [KeyLen]byte {
	if ap, ok := addrPortOf(addr); ok {
		o.mu.RLock()
		h, heard := o.heard[ap]
		k, found := o.peers[ap]
		o.mu.RUnlock()
		// whoever really speaks from the address is answered, as long as it still speaks: a dial
		// to the node expected there (that may be another node behind the same NAT) must not cut
		// the connection that lives on the address
		if heard && time.Since(h.at) < heardFor {
			return h.key
		}
		if found {
			return k
		}
	}
	return o.own
}

// noteHeard records that a packet from addr opened with key, so the answer is sealed with it.
func (o *NodeObfs) noteHeard(addr net.Addr, key *[KeyLen]byte) {
	ap, ok := addrPortOf(addr)
	if !ok {
		return
	}
	now := time.Now()
	o.mu.RLock()
	have, found := o.heard[ap]
	o.mu.RUnlock()
	if found && have.key == *key && now.Sub(have.at) < time.Second {
		return // the common case: nothing changes, no write lock per packet
	}
	o.mu.Lock()
	if len(o.heard) >= maxPeerKeys {
		o.heard = make(map[netip.AddrPort]heardKey)
	}
	o.heard[ap] = heardKey{key: *key, at: now}
	o.mu.Unlock()
}

// candidates are the keys a packet from addr may have been sealed with, most likely first: the
// key expected for exactly this address, other keys expected for this host (a peer may answer
// from another port), and the node's own key (for peers that dialed us).
func (o *NodeObfs) candidates(addr net.Addr, buf [][KeyLen]byte) [][KeyLen]byte {
	buf = buf[:0]
	if ap, ok := addrPortOf(addr); ok {
		o.mu.RLock()
		first, found := o.peers[ap]
		if found {
			buf = append(buf, first)
		}
		for _, k := range o.byIP[ap.Addr()] {
			if !found || k != first {
				buf = append(buf, k)
			}
		}
		o.mu.RUnlock()
	}
	return append(buf, o.own)
}

func keystreamXOR(key *[KeyLen]byte, salt, in, out []byte) {
	x, _ := blake2b.NewXOF(uint32(len(in)), key[:])
	x.Write(salt)
	_, _ = x.Read(out[:len(in)])
	for i := range in {
		out[i] ^= in[i]
	}
}

func tagOf(key *[KeyLen]byte, salt, plain []byte) [TagLen]byte {
	h, _ := blake2b.New256(key[:])
	h.Write([]byte("tag"))
	h.Write(salt)
	h.Write(plain)
	var sum [KeyLen]byte
	h.Sum(sum[:0])
	var t [TagLen]byte
	copy(t[:], sum[:TagLen])
	return t
}

// seal writes [salt][masked in][tag] to out and returns its length.
func seal(key *[KeyLen]byte, in, out, salt []byte) (int, error) {
	total := len(in) + Overhead
	if len(out) < total {
		return 0, ErrBufferTooSmall
	}
	copy(out, salt)
	keystreamXOR(key, out[:SaltLen], in, out[SaltLen:SaltLen+len(in)])
	tag := tagOf(key, out[:SaltLen], in)
	copy(out[SaltLen+len(in):], tag[:])
	return total, nil
}

// open restores the payload of in into out when in was sealed with key.
func open(key *[KeyLen]byte, in, out []byte) (int, bool) {
	if len(in) <= Overhead {
		return 0, false
	}
	n := len(in) - Overhead
	if len(out) < n {
		return 0, false
	}
	salt := in[:SaltLen]
	keystreamXOR(key, salt, in[SaltLen:SaltLen+n], out[:n])
	want := tagOf(key, salt, out[:n])
	if subtle.ConstantTimeCompare(want[:], in[SaltLen+n:]) != 1 {
		return 0, false
	}
	return n, true
}

// UDPLikePacketConn is the subset of *net.UDPConn that quic-go uses for kernel buffer sizing,
// GSO and path MTU discovery.
type UDPLikePacketConn interface {
	net.PacketConn
	SyscallConn() (syscall.RawConn, error)
	SetReadBuffer(bytes int) error
	SetWriteBuffer(bytes int) error
}

const maxPacketBufferSize = 4096

var _ net.PacketConn = (*ObfsPacketConn)(nil)

// ObfsPacketConn wraps a net.PacketConn and masks everything written to it and unmasks everything
// read from it. Packets that no known key opens are dropped silently.
type ObfsPacketConn struct {
	Conn net.PacketConn
	obfs *NodeObfs

	readMu  sync.Mutex
	readBuf []byte
	cands   [][KeyLen]byte

	writeMu  sync.Mutex
	writeBuf []byte
	saltBuf  [SaltLen]byte
}

// ObfsPacketConnUDP keeps the socket-tuning methods of a UDP-flavoured connection reachable.
type ObfsPacketConnUDP struct {
	*ObfsPacketConn
	UDPConn UDPLikePacketConn
}

// WrapPacketConn wraps conn with the node's obfuscation.
func WrapPacketConn(conn net.PacketConn, obfs *NodeObfs) net.PacketConn {
	base := &ObfsPacketConn{
		Conn:     conn,
		obfs:     obfs,
		readBuf:  make([]byte, maxPacketBufferSize),
		writeBuf: make([]byte, maxPacketBufferSize),
		cands:    make([][KeyLen]byte, 0, 10),
	}
	if udp, ok := conn.(UDPLikePacketConn); ok {
		return &ObfsPacketConnUDP{ObfsPacketConn: base, UDPConn: udp}
	}
	return base
}

func (c *ObfsPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		raw, addr, err := c.Conn.ReadFrom(c.readBuf)
		if err != nil {
			return 0, addr, err
		}
		c.cands = c.obfs.candidates(addr, c.cands)
		for i := range c.cands {
			if n, ok := open(&c.cands[i], c.readBuf[:raw], p); ok {
				c.obfs.noteHeard(addr, &c.cands[i])
				return n, addr, nil
			}
		}
		// not for us: a scanner, a stray packet, or a peer with another key
	}
}

func (c *ObfsPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	key := c.obfs.sendKey(addr)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := rand.Read(c.saltBuf[:]); err != nil {
		return 0, err
	}
	n, err := seal(&key, p, c.writeBuf, c.saltBuf[:])
	if err != nil {
		return 0, err
	}
	if _, err := c.Conn.WriteTo(c.writeBuf[:n], addr); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *ObfsPacketConn) Close() error                       { return c.Conn.Close() }
func (c *ObfsPacketConn) LocalAddr() net.Addr                { return c.Conn.LocalAddr() }
func (c *ObfsPacketConn) SetDeadline(t time.Time) error      { return c.Conn.SetDeadline(t) }
func (c *ObfsPacketConn) SetReadDeadline(t time.Time) error  { return c.Conn.SetReadDeadline(t) }
func (c *ObfsPacketConn) SetWriteDeadline(t time.Time) error { return c.Conn.SetWriteDeadline(t) }

func (u *ObfsPacketConnUDP) SetReadBuffer(bytes int) error  { return u.UDPConn.SetReadBuffer(bytes) }
func (u *ObfsPacketConnUDP) SetWriteBuffer(bytes int) error { return u.UDPConn.SetWriteBuffer(bytes) }
func (u *ObfsPacketConnUDP) SyscallConn() (syscall.RawConn, error) {
	return u.UDPConn.SyscallConn()
}
