// Package chatcrypto is the key agreement and the ratchet of chat/2 (docs/protocol/v2/services.md 4):
// the prekeys/2 a device publishes, PQXDH sessions and the Double Ratchet.
//
// The constructions are v1's (internal/crypto/x3dh, internal/crypto/keymanager) used on the spot
// (AD2) with the v2 key-schedule labels of services.md 4.0 and a random X3DH identity key per device
// (objects.md 5). Sessions and prekeys live only in the v2 store (AD7), never in the v1 database.
//
// The package does not touch the network: callers hand it verified objects (prekeys/2 and the
// device list that vouches for the device) and bytes of chat/2 messages.
package chatcrypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/crypto/x3dh"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	objpb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"github.com/echofog/echofog-core/internal/v2/store"
	"github.com/echofog/echofog-core/pkg/api/types"
	"golang.org/x/crypto/curve25519"
)

// Key schedule labels of chat/2 (services.md 4.0).
const (
	LabelPQXDH       = "echofog/chat2/pqxdh"
	LabelRatchetInit = "echofog/chat2/ratchet-init"
	LabelRatchetRoot = "echofog/chat2/ratchet-root"
	// StorageKeyInfo is the v2 name of the local storage key label (services.md 4.0).
	StorageKeyInfo = "echofog/storage2/key"
)

// Labels are the x3dh labels of a v2 session. The classic X3DH label is the PQXDH one: a classic
// init is refused in v2 (services.md 4.4), so the label is never used for a key.
var Labels = x3dh.Labels{
	Session:    LabelPQXDH,
	PQXDH:      LabelPQXDH,
	InitChains: LabelRatchetInit,
	Root:       LabelRatchetRoot,
}

// Store namespaces (AD13).
const (
	KeysNamespace     = "v2/chat/keys"
	SessionsNamespace = "v2/chat/sessions"
)

// Lifetimes (objects.md 5).
const (
	// SPKRotation: the signed prekey and the KEM prekey change every 7 days.
	SPKRotation = 7 * 24 * time.Hour
	// PrekeysLifetime is the lifetime of a published prekeys/2.
	PrekeysLifetime = 30 * 24 * time.Hour
	// PrekeysRenewBefore: the object is issued again when less than this remains.
	PrekeysRenewBefore = 10 * 24 * time.Hour
	// MaxPrekeyAge is the oldest signed prekey a sender still uses: a rotation plus the lifetime of
	// the object that carried it.
	MaxPrekeyAge = SPKRotation + PrekeysLifetime
)

var (
	// ErrRejected is every failure of an incoming init or ratchet message caused by the remote side:
	// a bad key, an unknown or used prekey, a message that does not open, a classic init, a sender
	// that the device list does not vouch for. One error, one text: a sender learns nothing from
	// which check failed (AD20).
	ErrRejected = errors.New("chat/2: message rejected")
	// ErrDuplicate is an init that was accepted before: a repeat, not an attack.
	ErrDuplicate = errors.New("chat/2: duplicate init")
	// ErrNoSession is Encrypt or Decrypt for a device without a session.
	ErrNoSession = errors.New("chat/2: no session")
	// ErrStale is a prekeys/2 that is expired or whose keys are too old to start a session with.
	ErrStale = errors.New("chat/2: stale prekeys")
	// ErrNotVouched is prekeys/2 from a device the account's device list does not list.
	ErrNotVouched = errors.New("chat/2: device not in the device list")
)

// Config configures Crypto.
type Config struct {
	// Device signs the prekeys/2 objects and is the local device.
	Device *identity.Device
	// Store opens a namespace of the v2 store (node.Host.Store).
	Store func(ns string) store.KV
	// IDs blinds peer identifiers in store keys (security.md 4.8).
	IDs *store.IDs
	// Now is the node clock (AD13). Prekey objects, their renewal and the freshness checks use it.
	// The rotation of the signed and KEM prekeys itself follows the wall clock inside the v1 key
	// manager, which has no injectable clock (see the report of T7.1).
	Now func() time.Time
}

// Record keys of the keys namespace besides the ones of the v1 key manager (spk:, kem:, opk:, jopk:).
const (
	rootKey    = "v2-root"    // 64 random bytes: the root of the key manager of this device
	ikKey      = "v2-ik"      // 32 random bytes: the X3DH identity DH key (objects.md 5)
	metaKey    = "v2-meta"    // when the current signed and KEM prekeys were first seen
	prekeysKey = "v2-prekeys" // the prekeys/2 envelope issued last
	bindPrefix = "bind:"      // sessions namespace: device -> account (node ID)
	prevPrefix = "prev:"      // sessions namespace: device -> end (unix seconds) of the kept replaced session
	clockSkew  = time.Minute  // tolerance when a creation time is in the future
	metaLen    = 32 + 8 + 32 + 8
)

// Crypto holds the prekeys and the sessions of the local device. All methods are safe for
// concurrent use; they run one at a time, which is also what makes a one-time prekey usable
// exactly once.
type Crypto struct {
	cfg      Config
	keysKV   store.KV
	sessKV   store.KV
	keysDB   *kvStorage
	sessDB   *kvStorage
	devID    [32]byte
	ikPub    [32]byte
	ikSecret *secret.Value

	mu     sync.Mutex
	closed bool
	acc    *keymanager.AccountManager
	eng    *x3dh.X3DHEngine
	sm     *x3dh.X3DHSessionManager
}

// ikSource gives the random X3DH identity DH key of the device to the v1 key manager and engine.
// It never returns a typed nil: a missing key is an error (AD17).
type ikSource struct {
	c *Crypto
}

func (s ikSource) GetEncryptionKey() (*memguard.Enclave, []byte, error) {
	if s.c == nil || s.c.ikSecret == nil {
		return nil, nil, errors.New("chat/2: identity DH key is not set")
	}
	var enc *memguard.Enclave
	err := s.c.ikSecret.Use(func(b []byte) error {
		if len(b) != 32 {
			return fmt.Errorf("chat/2: identity DH key of %d bytes", len(b))
		}
		enc = memguard.NewEnclave(append([]byte(nil), b...))
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return enc, append([]byte(nil), s.c.ikPub[:]...), nil
}

// New opens the prekeys and sessions of the device in the store, making them on first use.
func New(cfg Config) (*Crypto, error) {
	if cfg.Device == nil || cfg.Store == nil || cfg.IDs == nil || cfg.Now == nil {
		return nil, errors.New("chat/2: Device, Store, IDs and Now are required")
	}
	c := &Crypto{cfg: cfg, devID: cfg.Device.ID()}
	c.keysKV, c.sessKV = cfg.Store(KeysNamespace), cfg.Store(SessionsNamespace)
	c.keysDB = &kvStorage{kv: c.keysKV, ids: cfg.IDs}
	c.sessDB = &kvStorage{kv: c.sessKV, ids: cfg.IDs}

	root, err := c.loadOrMake(rootKey, 64)
	if err != nil {
		return nil, err
	}
	defer secret.Wipe(root)
	ik, err := c.loadOrMake(ikKey, 32)
	if err != nil {
		return nil, err
	}
	var priv [32]byte
	copy(priv[:], ik)
	secret.Wipe(ik)
	curve25519.ScalarBaseMult(&c.ikPub, &priv)
	c.ikSecret = secret.New(priv[:]) // wipes priv

	km := keymanager.NewKeyManager(root, c.keysDB)
	src := ikSource{c: c}
	acc, err := km.GetAccountWithOptions(0, keymanager.Options{StorageKeyInfo: StorageKeyInfo, IdentityKeySource: src})
	if err != nil {
		c.ikSecret.Destroy()
		return nil, err
	}
	c.acc = acc
	own := &keymanager.PublicBundle{PeerID: c.devID, IdentityKey: c.ikPub}
	c.eng = x3dh.NewX3DHEngineWithConfig(own, c.keysDB, acc, x3dh.Config{Labels: Labels, Identity: src})
	skey, err := acc.StorageKey()
	if err != nil {
		c.ikSecret.Destroy()
		return nil, err
	}
	c.sm = x3dh.NewX3DHSessionManager(c.sessDB, skey)
	return c, nil
}

// loadOrMake returns the n random bytes kept under name, making them on first use.
func (c *Crypto) loadOrMake(name string, n int) ([]byte, error) {
	b, err := c.keysKV.Get([]byte(name))
	if err == nil {
		if len(b) != n {
			return nil, fmt.Errorf("chat/2: record %s has %d bytes, want %d", name, len(b), n)
		}
		return b, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	b = make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := c.keysKV.Put([]byte(name), b); err != nil {
		secret.Wipe(b)
		return nil, err
	}
	return b, nil
}

// Close wipes the keys held in memory.
func (c *Crypto) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.ikSecret.Destroy()
}

// current returns the keys of the prekeys/2 now: the signed and KEM prekey with the time each was
// first seen. A new one gets the time of the node clock. The caller holds c.mu.
func (c *Crypto) current(now time.Time) (spk []byte, spkAt uint64, kem []byte, kemAt uint64, err error) {
	_, spk, err = c.acc.GetSignedPreKey()
	if err != nil {
		return nil, 0, nil, 0, err
	}
	kem, err = c.acc.KEMPreKey()
	if err != nil {
		return nil, 0, nil, 0, err
	}
	kemID := keymanager.KEMKeyID(kem)

	var meta [metaLen]byte
	if b, gerr := c.keysKV.Get([]byte(metaKey)); gerr == nil && len(b) == metaLen {
		copy(meta[:], b)
	}
	changed := false
	if !equal(meta[:32], spk) {
		copy(meta[:32], spk)
		binary.BigEndian.PutUint64(meta[32:40], uint64(now.Unix()))
		changed = true
	}
	if !equal(meta[40:72], kemID[:]) {
		copy(meta[40:72], kemID[:])
		binary.BigEndian.PutUint64(meta[72:80], uint64(now.Unix()))
		changed = true
	}
	if changed {
		if err := c.keysKV.Put([]byte(metaKey), meta[:]); err != nil {
			return nil, 0, nil, 0, err
		}
	}
	return spk, binary.BigEndian.Uint64(meta[32:40]), kem, binary.BigEndian.Uint64(meta[72:80]), nil
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var d byte
	for i := range a {
		d |= a[i] ^ b[i]
	}
	return d == 0
}

// seal builds and signs a prekeys/2 envelope with the given one-time prekeys.
func (c *Crypto) seal(now time.Time, seq uint64, spk []byte, spkAt uint64, kem []byte, kemAt uint64, opks [][]byte) ([]byte, error) {
	body, err := objects.Marshal(&objpb.Prekeys{
		Ik: keys.New(keys.X25519, c.ikPub[:]), Spk: spk, SpkCreated: spkAt, Kem: kem, KemCreated: kemAt, Opks: opks,
	})
	if err != nil {
		return nil, err
	}
	var raw []byte
	err = c.cfg.Device.WithSigner(func(sg *keys.Signer) error {
		var err error
		raw, err = objects.Seal(objects.LabelPrekeys, sg, seq, uint64(now.Add(PrekeysLifetime).Unix()), body)
		return err
	})
	return raw, err
}

// Prekeys returns the prekeys/2 envelope to publish (without one-time prekeys; objects.md 5): the
// current one while it names the current keys and stays valid for the renew margin, otherwise a
// new one with a higher seq.
func (c *Crypto) Prekeys() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errClosed
	}
	return c.prekeysLocked()
}

func (c *Crypto) prekeysLocked() ([]byte, error) {
	now := c.cfg.Now()
	spk, spkAt, kem, kemAt, err := c.current(now)
	if err != nil {
		return nil, err
	}
	var prevSeq uint64
	if raw, gerr := c.keysKV.Get([]byte(prekeysKey)); gerr == nil {
		if p, verr := objects.VerifyPrekeys(raw, now); verr == nil {
			prevSeq = p.Env.Seq
			left := time.Unix(int64(p.Env.NotAfter), 0).Sub(now)
			if left >= PrekeysRenewBefore && equal(p.Body.GetSpk(), spk) && equal(p.Body.GetKem(), kem) &&
				equal(p.Body.GetIk(), keys.New(keys.X25519, c.ikPub[:])) && len(p.Body.GetOpks()) == 0 {
				return raw, nil
			}
		} else if e, perr := objects.Parse(raw); perr == nil {
			prevSeq = e.Seq // expired, but its seq still counts
		}
	}
	seq := uint64(now.Unix())
	if seq <= prevSeq {
		seq = prevSeq + 1
	}
	raw, err := c.seal(now, seq, spk, spkAt, kem, kemAt, nil)
	if err != nil {
		return nil, err
	}
	if err := c.keysKV.Put([]byte(prekeysKey), raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// PrekeysHash is SHA-256 of the current Prekeys envelope: the card's prekeys_hash.
func (c *Crypto) PrekeysHash() ([]byte, error) {
	raw, err := c.Prekeys()
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(raw)
	return h[:], nil
}

// Bundle returns a prekeys/2 with one fresh one-time prekey, for a peer that asks directly
// (BUNDLE, services.md 4.1). The one-time prekey is used up by the first init that names it. The
// number of such prekeys kept is bounded by the v1 key manager (MaxJITOPKs, JITOPKRetention).
func (c *Crypto) Bundle() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errClosed
	}
	base, err := c.prekeysLocked()
	if err != nil {
		return nil, err
	}
	now := c.cfg.Now()
	p, err := objects.VerifyPrekeys(base, now)
	if err != nil {
		return nil, err
	}
	// the key manager makes, stores and counts the prekey; its own signed bundle is not used
	jit, err := c.acc.CreateJITBundle()
	if err != nil || len(jit.OneTimeKeys) != 1 {
		return nil, fmt.Errorf("chat/2: one-time prekey: %v", err)
	}
	opk := jit.OneTimeKeys[0]
	return c.seal(now, p.Env.Seq, p.Body.GetSpk(), p.Body.GetSpkCreated(), p.Body.GetKem(), p.Body.GetKemCreated(), [][]byte{opk[:]})
}

// bind records which account a session's device belongs to.
func (c *Crypto) bind(device, node [32]byte) error {
	return c.sessKV.Put([]byte(bindPrefix+c.sessDB.BlindID(hex.EncodeToString(device[:]))), node[:])
}

// Peer returns the account (node ID) the session with device is bound to.
func (c *Crypto) Peer(device [32]byte) (node [32]byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return node, false
	}
	b, err := c.sessKV.Get([]byte(bindPrefix + c.sessDB.BlindID(hex.EncodeToString(device[:]))))
	if err != nil || len(b) != 32 {
		return node, false
	}
	if _, err := c.sm.Load(types.PeerID(device)); err != nil {
		return node, false
	}
	copy(node[:], b)
	return node, true
}

// PrevRetention is how long a replaced session stays for messages still on the way (services.md 4.4).
const PrevRetention = 7 * 24 * time.Hour

// prevID is the session-manager key under which the replaced session of device is kept.
func prevID(device [32]byte) types.PeerID {
	return types.PeerID(sha256.Sum256(append([]byte("echofog/chat2/prev"), device[:]...)))
}

func (c *Crypto) prevKey(device [32]byte) []byte {
	return []byte(prevPrefix + c.sessDB.BlindID(hex.EncodeToString(device[:])))
}

// keepPrev moves the current session with device, if there is one, to the replaced slot: a session
// replaced by a fresh one still opens messages sent before the replacement for 7 days. c.mu is held.
func (c *Crypto) keepPrev(device [32]byte) error {
	old, err := c.sm.Load(types.PeerID(device))
	if err != nil {
		return nil // no session to keep
	}
	if err := c.sm.Save(prevID(device), old); err != nil {
		return err
	}
	end := uint64(c.cfg.Now().Add(PrevRetention).Unix())
	return c.sessKV.Put(c.prevKey(device), binary.BigEndian.AppendUint64(nil, end))
}

// dropPrev forgets the replaced session of device. c.mu is held.
func (c *Crypto) dropPrev(device [32]byte) {
	_ = c.sm.Delete(prevID(device))
	_ = c.sessKV.Delete(c.prevKey(device))
}

// loadPrev returns the replaced session of device while it is within its 7 days; an expired one is
// deleted. c.mu is held.
func (c *Crypto) loadPrev(device [32]byte) *x3dh.X3DHSession {
	b, err := c.sessKV.Get(c.prevKey(device))
	if err != nil || len(b) != 8 {
		return nil
	}
	if c.cfg.Now().Unix() >= int64(binary.BigEndian.Uint64(b)) {
		c.dropPrev(device)
		return nil
	}
	sess, err := c.sm.Load(prevID(device))
	if err != nil {
		return nil
	}
	return sess
}

// ikOf reads the identity DH key of a prekeys/2 object.
func ikOf(p *objects.Prekeys) ([32]byte, error) {
	var out [32]byte
	k, err := keys.ParseKind(p.Body.GetIk(), keys.X25519)
	if err != nil || len(k.Key()) != 32 {
		return out, errors.New("chat/2: bad identity DH key")
	}
	copy(out[:], k.Key())
	return out, nil
}

// Start begins a session with the device that signed peer, which dl vouches for, and returns the
// X3DHInit message carrying first (the first plaintext). Every start is PQXDH.
func (c *Crypto) Start(peer *objects.Prekeys, dl *objects.DeviceList, first []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errClosed
	}
	if peer == nil || peer.Env == nil || peer.Body == nil || dl == nil {
		return nil, ErrStale
	}
	if peer.DeviceID == c.devID {
		return nil, errors.New("chat/2: a session with the own device")
	}
	if !peer.VouchedBy(dl) {
		return nil, ErrNotVouched
	}
	now := c.cfg.Now()
	if _, err := objects.VerifyPrekeys(peer.Env.Raw, now); err != nil {
		return nil, ErrStale
	}
	for _, at := range []uint64{peer.Body.GetSpkCreated(), peer.Body.GetKemCreated()} {
		t := time.Unix(int64(at), 0)
		if at == 0 || t.After(now.Add(clockSkew)) || now.Sub(t) > MaxPrekeyAge {
			return nil, ErrStale
		}
	}
	ik, err := ikOf(peer)
	if err != nil {
		return nil, ErrStale
	}
	bundle := keymanager.PublicBundle{
		PeerID:       peer.DeviceID,
		IdentityKey:  ik,
		SignedPreKey: [32]byte(peer.Body.GetSpk()),
		KEMPreKey:    append([]byte(nil), peer.Body.GetKem()...),
		CreatedAt:    int64(peer.Env.Seq),
	}
	var opks [][32]byte
	for _, o := range peer.OPKs() {
		opks = append(opks, [32]byte(o))
	}
	bundle.OneTimeKeys = c.sm.UnusedOPKs(types.PeerID(peer.DeviceID), opks)

	sess, pkt, err := c.eng.X3DHInit(bundle)
	if err != nil {
		return nil, err
	}
	msg, err := sess.RatchetEncrypt(first)
	if err != nil {
		return nil, err
	}
	m := &chatpb.X3DHInit{
		EphemeralKey: pkt.EphemeralKey[:],
		IdentityKey:  pkt.IdentityKey[:],
		InitialCiphertext: &chatpb.SecureMessage{
			Header:     &chatpb.RatchetHeader{RatchetKey: msg.Header.DH[:], Pn: msg.Header.PN, N: msg.Header.N},
			Ciphertext: msg.Ciphertext,
		},
		KemCiphertext: pkt.KEMCiphertext,
	}
	if pkt.KEMKeyID != nil {
		m.KemKeyId = pkt.KEMKeyID[:]
	}
	if pkt.OneTimeKeyID != nil {
		m.OneTimeKeyId = pkt.OneTimeKeyID[:]
	}
	out, err := objects.Marshal(m)
	if err != nil {
		return nil, err
	}
	if _, err := chatpb.ParseX3DHInit(out); err != nil {
		return nil, err // a bug: Start must never build what the parser refuses
	}
	if err := c.keepPrev(peer.DeviceID); err != nil {
		return nil, err
	}
	if err := c.sm.Save(types.PeerID(peer.DeviceID), sess); err != nil {
		return nil, err
	}
	if pkt.OneTimeKeyID != nil {
		c.sm.MarkOPKUsed(types.PeerID(peer.DeviceID), *pkt.OneTimeKeyID)
	}
	if err := c.bind(peer.DeviceID, dl.NodeID); err != nil {
		return nil, err
	}
	return out, nil
}

// Accept opens an incoming X3DHInit from the device that signed sender (vouched for by dl), starts
// the session and returns the first plaintext. Every failure caused by the init or the sender is
// ErrRejected (one text, AD20) and leaves no trace: no session, no used one-time prekey. An init
// that was accepted before is ErrDuplicate.
func (c *Crypto) Accept(sender *objects.Prekeys, dl *objects.DeviceList, init []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errClosed
	}
	m, err := chatpb.ParseX3DHInit(init)
	if err != nil || sender == nil || sender.Env == nil || sender.Body == nil || dl == nil {
		return nil, ErrRejected
	}
	if !sender.VouchedBy(dl) || sender.DeviceID == c.devID {
		return nil, ErrRejected
	}
	if _, err := objects.VerifyPrekeys(sender.Env.Raw, c.cfg.Now()); err != nil {
		return nil, ErrRejected
	}
	ik, err := ikOf(sender)
	if err != nil || ik != [32]byte(m.GetIdentityKey()) {
		return nil, ErrRejected
	}
	hdr := m.GetInitialCiphertext().GetHeader()
	pkt := types.X3DHInit{
		EphemeralKey: [32]byte(m.GetEphemeralKey()),
		IdentityKey:  ik,
		SecureMessage: types.EncryptedMessage{
			Header:     types.RatchetHeader{DH: types.RatchetPublicKey(hdr.GetRatchetKey()), PN: hdr.GetPn(), N: hdr.GetN()},
			Ciphertext: m.GetInitialCiphertext().GetCiphertext(),
		},
		KEMCiphertext: m.GetKemCiphertext(),
	}
	kemID := [32]byte(m.GetKemKeyId())
	pkt.KEMKeyID = &kemID
	if o := m.GetOneTimeKeyId(); len(o) == 32 {
		k := [32]byte(o)
		pkt.OneTimeKeyID = &k
	}
	if c.sm.InitSeen(pkt.EphemeralKey) {
		return nil, ErrDuplicate
	}
	bundle := keymanager.PublicBundle{PeerID: sender.DeviceID, IdentityKey: ik}
	sess, plain, err := c.eng.Accept(pkt, bundle)
	if err != nil {
		return nil, ErrRejected
	}
	if err := c.keepPrev(sender.DeviceID); err != nil {
		return nil, err
	}
	if err := c.sm.Save(types.PeerID(sender.DeviceID), sess); err != nil {
		return nil, err
	}
	c.sm.MarkInitSeen(pkt.EphemeralKey)
	if err := c.bind(sender.DeviceID, dl.NodeID); err != nil {
		return nil, err
	}
	return plain, nil
}

// Encrypt encrypts plain to the device of an existing session and returns a SecureMessage.
func (c *Crypto) Encrypt(device [32]byte, plain []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errClosed
	}
	sess, err := c.sm.Load(types.PeerID(device))
	if err != nil {
		return nil, ErrNoSession
	}
	msg, err := sess.RatchetEncrypt(plain)
	if err != nil {
		return nil, err
	}
	out, err := objects.Marshal(&chatpb.SecureMessage{
		Header:     &chatpb.RatchetHeader{RatchetKey: msg.Header.DH[:], Pn: msg.Header.PN, N: msg.Header.N},
		Ciphertext: msg.Ciphertext,
	})
	if err != nil {
		return nil, err
	}
	// the chain moved on: store it before the message leaves
	if err := c.sm.Save(types.PeerID(device), sess); err != nil {
		return nil, err
	}
	return out, nil
}

// Decrypt opens a SecureMessage from the device of an existing session. A failure never touches
// the session (K8): the session is stored only after the message opened.
func (c *Crypto) Decrypt(device [32]byte, msg []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errClosed
	}
	sess, err := c.sm.Load(types.PeerID(device))
	if err != nil {
		return nil, ErrNoSession
	}
	m, err := chatpb.ParseSecureMessage(msg)
	if err != nil {
		return nil, ErrRejected
	}
	em := types.EncryptedMessage{
		Header:     types.RatchetHeader{DH: types.RatchetPublicKey(m.GetHeader().GetRatchetKey()), PN: m.GetHeader().GetPn(), N: m.GetHeader().GetN()},
		Ciphertext: m.GetCiphertext(),
	}
	plain, err := sess.RatchetDecrypt(em)
	if err != nil {
		// a message sent before the session was replaced
		prev := c.loadPrev(device)
		if prev == nil {
			return nil, ErrRejected
		}
		if plain, err = prev.RatchetDecrypt(em); err != nil {
			return nil, ErrRejected
		}
		if err := c.sm.Save(prevID(device), prev); err != nil {
			return nil, err
		}
		return plain, nil
	}
	if err := c.sm.Save(types.PeerID(device), sess); err != nil {
		return nil, err
	}
	return plain, nil
}
