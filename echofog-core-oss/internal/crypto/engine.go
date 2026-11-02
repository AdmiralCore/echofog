package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/crypto/x3dh"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type BundleSource interface {
	GetBundle(peerID types.PeerID) (*keymanager.PublicBundle, error)
}

type CryptoEngine struct {
	me           types.PeerID
	accManager   *keymanager.AccountManager
	x3dhEngine   *x3dh.X3DHEngine
	sessionStore *x3dh.X3DHSessionManager
	bundleSource BundleSource

	mu sync.RWMutex

	// peers whose bundle lost its KEM prekey: no new lookup for them until the wait ends
	waitMu  sync.Mutex
	keyWait map[types.PeerID]keyWait
	now     func() time.Time

	log *slog.Logger
}

// keyWait is how long a peer's bundle is not looked up again after one without its KEM prekey came.
type keyWait struct {
	until time.Time
	pause time.Duration
}

// After a bundle that lost its KEM prekey, the peer's bundle is looked up again only after
// keyWaitFirst, doubling up to keyWaitMax while the same keeps coming. An away peer's old
// classic record in the DHT would otherwise cost a lookup for every retry of every message to it.
const (
	keyWaitFirst = 30 * time.Second
	keyWaitMax   = 15 * time.Minute
)

func NewCryptoEngine(
	me types.PeerID,
	accManager *keymanager.AccountManager,
	x3dhEngine *x3dh.X3DHEngine,
	sessionStore *x3dh.X3DHSessionManager,
	bundleSource BundleSource,
	log *slog.Logger,
) *CryptoEngine {
	return &CryptoEngine{
		me:           me,
		accManager:   accManager,
		x3dhEngine:   x3dhEngine,
		sessionStore: sessionStore,
		bundleSource: bundleSource,
		now:          time.Now,
		log:          log,
	}
}

var ErrInitReplay = errors.New("this init packet was accepted before")

// ErrPQDowngrade: the peer's bundle has no KEM prekey although we saw one of the same age or newer
// with it. The bundle is dropped from the cache; the message waits for a fresh bundle, which is
// looked up again only after a pause (see keyWaitFirst).
var ErrPQDowngrade = errors.New("the peer's bundle lost its KEM prekey: waiting for a fresh one")

// bundleForgetter is a BundleSource that caches bundles and can drop one (the swarm does).
type bundleForgetter interface {
	ForgetBundle(peerID types.PeerID)
}

// liveSource is a BundleSource that can tell a peer connected now: its bundle then comes from the
// peer itself, fresh and without a lookup in the network, so a wait for it does not hold.
type liveSource interface {
	Connected(peerID types.PeerID) bool
}

// waitingForKey reports whether a peer's bundle is not to be looked up now: one without its KEM
// prekey came a moment ago, and the peer is not connected to hand us a fresh one.
func (e *CryptoEngine) waitingForKey(peer types.PeerID) bool {
	e.waitMu.Lock()
	w, ok := e.keyWait[peer]
	e.waitMu.Unlock()
	if !ok || !e.now().Before(w.until) {
		return false
	}
	if l, ok := e.bundleSource.(liveSource); ok && l.Connected(peer) {
		return false
	}
	return true
}

// waitForKey starts or doubles the wait for a peer's fresh bundle. The first one is logged; the
// lookups it saves are not.
func (e *CryptoEngine) waitForKey(peer types.PeerID) {
	e.waitMu.Lock()
	if e.keyWait == nil {
		e.keyWait = make(map[types.PeerID]keyWait)
	}
	w, again := e.keyWait[peer]
	w.pause = min(max(2*w.pause, keyWaitFirst), keyWaitMax)
	w.until = e.now().Add(w.pause)
	if len(e.keyWait) >= 4096 && !again {
		clear(e.keyWait) // a pause saved, never one needed
	}
	e.keyWait[peer] = w
	e.waitMu.Unlock()
	if !again && e.log != nil {
		e.log.Warn("the peer's key bundle lost its post-quantum key: waiting for a fresh one before writing to it",
			"peer", peer.String()[:8], "next_lookup_in", w.pause)
	}
}

// BundleCame takes a peer's bundle that reached us outside a lookup (a card it sent us, checked by
// the caller): one with a KEM prekey tells the peer is on PQXDH, and either ends a wait for its
// fresh bundle.
func (e *CryptoEngine) BundleCame(peer types.PeerID, b *keymanager.PublicBundle) {
	if b.HasKEM() {
		e.sessionStore.NotePQBundle(peer, b.CreatedAt)
	}
	e.keyCame(peer)
}

// keyCame ends the wait for a peer's fresh bundle.
func (e *CryptoEngine) keyCame(peer types.PeerID) {
	e.waitMu.Lock()
	delete(e.keyWait, peer)
	e.waitMu.Unlock()
}

func (e *CryptoEngine) EncryptMessage(toPeer types.PeerID, plaintext []byte) (types.EncryptedMessage, *types.X3DHInit, error) {
	// a missing bundle is looked up in the network: not under the lock, that can take seconds
	var bundle *keymanager.PublicBundle
	if has, _ := e.HasSession(toPeer); !has {
		if e.waitingForKey(toPeer) {
			return types.EncryptedMessage{}, nil, ErrPQDowngrade
		}
		var err error
		if bundle, err = e.bundleSource.GetBundle(toPeer); err != nil {
			return types.EncryptedMessage{}, nil, fmt.Errorf("peer bundle not found: %w", err)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	var initPacket *types.X3DHInit

	session, err := e.sessionStore.Load(toPeer)
	if err != nil {
		if bundle == nil { // the session went away meanwhile
			return types.EncryptedMessage{}, nil, errors.New("session was reset, try again")
		}

		// once a peer offered a KEM prekey, a bundle of it without one that is not newer is a
		// stripped or replayed copy: drop it and wait for a fresh one rather than go classic. The
		// peer would refuse the classic init anyway, but its first message would have gone out
		// under a classic key. While the same copy is all the network has (an away peer's old
		// record), it is not looked up again for every retry: see waitForKey.
		if bundle.HasKEM() {
			e.sessionStore.NotePQBundle(toPeer, bundle.CreatedAt)
			e.keyCame(toPeer)
		} else if seen := e.sessionStore.PQBundleSeen(toPeer); seen != 0 && bundle.CreatedAt <= seen {
			if f, ok := e.bundleSource.(bundleForgetter); ok {
				f.ForgetBundle(toPeer)
			}
			e.waitForKey(toPeer)
			return types.EncryptedMessage{}, nil, ErrPQDowngrade
		}

		// a bundle from cache may offer one-time prekeys we used already; the peer deleted them
		b := *bundle
		b.OneTimeKeys = e.sessionStore.UnusedOPKs(toPeer, bundle.OneTimeKeys)

		var packet types.X3DHInit
		session, packet, err = e.x3dhEngine.X3DHInit(b)
		if err != nil {
			return types.EncryptedMessage{}, nil, fmt.Errorf("X3DH init failed: %w", err)
		}
		if packet.OneTimeKeyID != nil {
			e.sessionStore.MarkOPKUsed(toPeer, *packet.OneTimeKeyID)
		}
		initPacket = &packet
	}

	encryptedMsg, err := session.RatchetEncrypt(plaintext)
	if err != nil {
		return types.EncryptedMessage{}, nil, fmt.Errorf("ratchet encrypt failed: %w", err)
	}

	if err = e.sessionStore.Save(toPeer, session); err != nil {
		return types.EncryptedMessage{}, nil, fmt.Errorf("failed to save session: %w", err)
	}

	return encryptedMsg, initPacket, nil
}

func (e *CryptoEngine) HandleIncomingMessage(fromPeer types.PeerID, msg types.EncryptedMessage) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	session, err := e.sessionStore.Load(fromPeer)
	if err != nil {
		return nil, fmt.Errorf("session not found (did we miss init packet?): %w", err)
	}

	plaintext, err := session.RatchetDecrypt(msg)
	if err != nil {
		return nil, fmt.Errorf("decrypt failed: %w", err)
	}

	if err = e.sessionStore.Save(fromPeer, session); err != nil {
		return nil, fmt.Errorf("save state failed: %w", err)
	}

	return plaintext, nil
}

// HandleInitPacket opens the message of an init packet from fromPeer. A repeat of the packet that
// started the current session is read with that session. Otherwise X3DH runs against the
// sender's signed bundle and the new session replaces ours, unless keepExisting: then the message
// is read and the new session thrown away (both sides started one at once and ours wins).
//
// senderBundle is the sender's bundle that came with the packet (checked by the caller); without it
// the bundle is looked up.
func (e *CryptoEngine) HandleInitPacket(fromPeer types.PeerID, initPacket types.X3DHInit, keepExisting bool, senderBundle *keymanager.PublicBundle) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	current, loadErr := e.sessionStore.Load(fromPeer)
	if loadErr == nil && !current.IsInitiator && current.InitEK == initPacket.EphemeralKey {
		if plaintext, err := current.RatchetDecrypt(initPacket.SecureMessage); err == nil {
			return plaintext, e.sessionStore.Save(fromPeer, current)
		}
		return current.ReadInitAgain(initPacket.SecureMessage)
	}

	if e.sessionStore.InitSeen(initPacket.EphemeralKey) {
		return nil, ErrInitReplay
	}

	bundle := senderBundle
	if bundle == nil {
		// looked up without the lock; the state is checked again below
		e.mu.Unlock()
		var err error
		bundle, err = e.bundleSource.GetBundle(fromPeer)
		e.mu.Lock()
		if err != nil {
			return nil, fmt.Errorf("cannot find initiator bundle: %w", err)
		}
		if e.sessionStore.InitSeen(initPacket.EphemeralKey) {
			return nil, ErrInitReplay
		}
		_, loadErr = e.sessionStore.Load(fromPeer)
	}

	session, plaintext, err := e.x3dhEngine.Accept(initPacket, *bundle)
	if err != nil {
		return nil, fmt.Errorf("X3DH receive failed: %w", err)
	}
	e.sessionStore.MarkInitSeen(initPacket.EphemeralKey)
	if bundle.HasKEM() { // the sender's signed card: it is on PQXDH, for our own next session
		e.sessionStore.NotePQBundle(fromPeer, bundle.CreatedAt)
		e.keyCame(fromPeer)
	}

	if keepExisting && loadErr == nil {
		return plaintext, nil
	}
	if err = e.sessionStore.Save(fromPeer, session); err != nil {
		return nil, err
	}
	return plaintext, nil
}

func (e *CryptoEngine) EncryptBlock(data []byte, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	return gcm.Seal(nonce, nonce, data, nil), nil
}

func (e *CryptoEngine) DecryptBlock(ciphertext []byte, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, actualCiphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, actualCiphertext, nil)
}

type X3DHInitializer struct {
	sessionStore *x3dh.X3DHSessionManager
	x3dhEng      *x3dh.X3DHEngine
}

// NewX3DHInitializer sets up X3DH for account. Sessions are sealed with a key derived from the
// account; the password argument is not used any more.
func NewX3DHInitializer(account *keymanager.AccountManager, storage storage.Storage, _ []byte) (*X3DHInitializer, error) {
	myBundle, err := account.CreateMyBundle(nil)
	if err != nil {
		return nil, err
	}
	key, err := account.StorageKey()
	if err != nil {
		return nil, err
	}
	return &X3DHInitializer{
		sessionStore: x3dh.NewX3DHSessionManager(storage, key),
		x3dhEng:      x3dh.NewX3DHEngine(myBundle, storage, account),
	}, nil
}

func (x *X3DHInitializer) GetSessionManager() *x3dh.X3DHSessionManager {
	return x.sessionStore
}

func (x *X3DHInitializer) GetEngine() *x3dh.X3DHEngine {
	return x.x3dhEng
}

func (e *CryptoEngine) HasSession(id types.PeerID) (hasSession bool, isInitiator bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	session, err := e.sessionStore.Load(id)
	if err != nil {
		return false, false
	}
	return true, session.IsInitiator
}

func (e *CryptoEngine) DeleteSession(id types.PeerID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessionStore.Delete(id)
}
