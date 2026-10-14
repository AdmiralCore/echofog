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

	log *slog.Logger
}

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
		log:          log,
	}
}

var ErrInitReplay = errors.New("this init packet was accepted before")

func (e *CryptoEngine) EncryptMessage(toPeer types.PeerID, plaintext []byte) (types.EncryptedMessage, *types.X3DHInit, error) {
	// a missing bundle is looked up in the network: not under the lock, that can take seconds
	var bundle *keymanager.PublicBundle
	if has, _ := e.HasSession(toPeer); !has {
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
