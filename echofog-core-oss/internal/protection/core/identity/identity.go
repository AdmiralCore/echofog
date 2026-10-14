package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"google.golang.org/protobuf/proto"
)

var (
	ErrIdentityLocked = errors.New("identity is locked or panic button activated")
	ErrKeyManagerNil  = errors.New("key manager is nil")
	ErrInvalidPass    = errors.New("invalid passphrase or corrupted data")
)

type identityManager struct {
	KeyManager *keymanager.KeyManager
	storage    storage.Storage

	activeAccountIndex uint32
	isUnlocked         bool

	cachedPrivKey ed25519.PrivateKey
	cachedPubKey  []byte
	cachedPeerID  types.PeerID

	mu sync.RWMutex
}

func newIdentityManager(storage storage.Storage) *identityManager {
	return &identityManager{
		storage:    storage,
		isUnlocked: false,
	}
}

func (m *identityManager) refreshCachedKeys() error {
	if !m.isUnlocked || m.KeyManager == nil {
		return ErrIdentityLocked
	}
	account, err := m.KeyManager.GetAccount(m.activeAccountIndex)
	if err != nil {
		return err
	}
	privEnc, pubKey, err := account.GetIdentityKey()
	if err != nil {
		return err
	}

	lockedPriv, err := privEnc.Open()
	if err != nil {
		return err
	}
	defer lockedPriv.Destroy()

	if len(m.cachedPrivKey) > 0 {
		memguard.WipeBytes(m.cachedPrivKey)
	}
	m.cachedPrivKey = make(ed25519.PrivateKey, len(lockedPriv.Bytes()))
	copy(m.cachedPrivKey, lockedPriv.Bytes())

	m.cachedPubKey = make([]byte, len(pubKey))
	copy(m.cachedPubKey, pubKey)

	var peerPubKey types.PeerPublicKey
	copy(peerPubKey[:], pubKey)
	m.cachedPeerID = types.PeerPubKeyToID(peerPubKey)
	return nil
}

func (m *identityManager) RestoreFromMnemonic(mnemonic []byte, passphrase []byte) error {
	defer memguard.WipeBytes(mnemonic)
	defer memguard.WipeBytes(passphrase)

	m.mu.Lock()
	defer m.mu.Unlock()

	var err error
	m.KeyManager, err = keymanager.NewKeyManagerWithMnemonic(mnemonic, passphrase, m.storage)
	if err != nil {
		return err
	}

	m.isUnlocked = true
	_ = m.refreshCachedKeys()
	return nil
}

func (m *identityManager) GenerateNewIdentity(passphrase []byte) (*ProtectedMnemonic, error) {
	mnemonicEnclave, err := keymanager.GenerateSecureMnemonic()
	if err != nil {
		return nil, err
	}

	mnemonic, err := mnemonicEnclave.Open()
	if err != nil {
		return nil, err
	}
	protectedM := &ProtectedMnemonic{
		mnemonic: mnemonic,
	}

	go func(pm *ProtectedMnemonic) {
		time.Sleep(120 * time.Second)
		_ = pm.Close()
	}(protectedM)

	rootIEnclave, err := keymanager.SeedFromMnemonic(mnemonic.Bytes(), passphrase)
	if err != nil {
		return nil, err
	}

	rootI, err := rootIEnclave.Open()
	if err != nil {
		return nil, err
	}
	defer rootI.Destroy()

	m.mu.Lock()
	m.KeyManager = keymanager.NewKeyManager(rootI.Bytes(), m.storage)
	m.isUnlocked = true
	_ = m.refreshCachedKeys()
	m.mu.Unlock()

	return protectedM, nil
}

func (m *identityManager) SaveToStorage(passphrase []byte) error {
	defer memguard.WipeBytes(passphrase)

	if m.KeyManager == nil {
		return ErrKeyManagerNil
	}

	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return err
	}

	encryptedKey := argon2.IDKey(passphrase, salt, 1, 64*1024, 4, 32)
	defer memguard.WipeBytes(encryptedKey)

	aead, err := chacha20poly1305.NewX(encryptedKey)
	if err != nil {
		return err
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}

	masterKeyEnc, masterCCEnc := m.KeyManager.GetSecretKeys()

	lockedMK, err := masterKeyEnc.Open()
	if err != nil {
		return err
	}
	defer lockedMK.Destroy()

	lockedCC, err := masterCCEnc.Open()
	if err != nil {
		return err
	}
	defer lockedCC.Destroy()

	combinedBuf := memguard.NewBufferFromBytes(append(lockedMK.Bytes(), lockedCC.Bytes()...))
	defer combinedBuf.Destroy()

	encryptedData := aead.Seal(nil, nonce, combinedBuf.Bytes(), nil)

	protoToSave := &IdentityKeysInStorage{
		EncryptedData: encryptedData,
		Salt:          salt,
		Nonce:         nonce,
		Algorithm:     "argon2id-chacha20",
	}

	dataToSave, err := proto.Marshal(protoToSave)
	if err != nil {
		memguard.WipeBytes(encryptedData)
		return err
	}

	return m.storage.Set([]byte("system:identity:root"), dataToSave)
}

func (m *identityManager) LoadFromStorage(passphrase []byte, isDuress bool) error {
	defer memguard.WipeBytes(passphrase)

	if isDuress {
		slog.Warn("DURESS PASSWORD ACTIVATED. Initiating silent wipe...")
		m.PanicButton()
		fakeSeed := memguard.NewBuffer(64)
		defer fakeSeed.Destroy()
		m.KeyManager = keymanager.NewKeyManager(fakeSeed.Bytes(), m.storage)
		m.isUnlocked = true
		return nil
	}

	savedProto, err := m.storage.Get([]byte("system:identity:root"))
	if err != nil {
		return err
	}
	defer memguard.WipeBytes(savedProto)

	var protoKeys IdentityKeysInStorage
	if err = proto.Unmarshal(savedProto, &protoKeys); err != nil {
		return err
	}

	encryptedKey := argon2.IDKey(passphrase, protoKeys.Salt, 1, 64*1024, 4, 32)
	defer memguard.WipeBytes(encryptedKey)

	aead, err := chacha20poly1305.NewX(encryptedKey)
	if err != nil {
		return err
	}

	decryptedData, err := aead.Open(nil, protoKeys.Nonce, protoKeys.EncryptedData, nil)
	if err != nil {
		return ErrInvalidPass
	}
	defer memguard.WipeBytes(decryptedData)

	if len(decryptedData) != 64 {
		return errors.New("invalid key length in storage")
	}

	m.mu.Lock()
	m.KeyManager = keymanager.NewKeyManager(decryptedData, m.storage)
	m.isUnlocked = true
	_ = m.refreshCachedKeys()
	m.mu.Unlock()

	return nil
}

func (m *identityManager) PanicButton() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.cachedPrivKey) > 0 {
		memguard.WipeBytes(m.cachedPrivKey)
		m.cachedPrivKey = nil
	}
	m.cachedPubKey = nil
	m.cachedPeerID = types.PeerID{}
	if m.KeyManager != nil {
		m.KeyManager.Destroy()
		m.KeyManager = nil
	}
	m.isUnlocked = false
	m.activeAccountIndex = 0

	memguard.Purge()
}

func (m *identityManager) Sign(data []byte) ([]byte, error) {
	m.mu.RLock()
	if !m.isUnlocked || len(m.cachedPrivKey) == 0 {
		m.mu.RUnlock()
		m.mu.Lock()
		if !m.isUnlocked || m.KeyManager == nil {
			m.mu.Unlock()
			return nil, ErrIdentityLocked
		}
		if len(m.cachedPrivKey) == 0 {
			if err := m.refreshCachedKeys(); err != nil {
				m.mu.Unlock()
				return nil, err
			}
		}
		sig := ed25519.Sign(m.cachedPrivKey, data)
		m.mu.Unlock()
		return sig, nil
	}
	defer m.mu.RUnlock()

	return ed25519.Sign(m.cachedPrivKey, data), nil
}

func (m *identityManager) SignWalletTransaction(data []byte) ([]byte, error) {
	if !m.isUnlocked || m.KeyManager == nil {
		return nil, ErrIdentityLocked
	}

	account, err := m.KeyManager.GetAccount(m.activeAccountIndex)
	if err != nil {
		return nil, err
	}

	walletEnc, err := account.GetWalletKey()
	if err != nil {
		return nil, err
	}

	lockedWallet, err := walletEnc.Open()
	if err != nil {
		return nil, err
	}
	defer lockedWallet.Destroy()

	rawPriv := ed25519.NewKeyFromSeed(lockedWallet.Bytes())
	defer memguard.WipeBytes(rawPriv)

	return ed25519.Sign(rawPriv, data), nil
}

func (m *identityManager) GetWalletPublicKey() ([]byte, error) {
	if !m.isUnlocked || m.KeyManager == nil {
		return nil, ErrIdentityLocked
	}
	account, err := m.KeyManager.GetAccount(m.activeAccountIndex)
	if err != nil {
		return nil, err
	}
	walletEnc, err := account.GetWalletKey()
	if err != nil {
		return nil, err
	}
	locked, err := walletEnc.Open()
	if err != nil {
		return nil, err
	}
	defer locked.Destroy()

	rawPriv := ed25519.NewKeyFromSeed(locked.Bytes())
	defer memguard.WipeBytes(rawPriv)

	pub := rawPriv.Public().(ed25519.PublicKey)
	pubCopy := make([]byte, len(pub))
	copy(pubCopy, pub)
	return pubCopy, nil
}

func (m *identityManager) GetOwnPeerID() (types.PeerID, error) {
	m.mu.RLock()
	if m.isUnlocked && m.cachedPeerID != (types.PeerID{}) {
		defer m.mu.RUnlock()
		return m.cachedPeerID, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isUnlocked || m.KeyManager == nil {
		return types.PeerID{}, ErrIdentityLocked
	}
	if m.cachedPeerID != (types.PeerID{}) {
		return m.cachedPeerID, nil
	}
	if err := m.refreshCachedKeys(); err != nil {
		return types.PeerID{}, err
	}
	return m.cachedPeerID, nil
}

func (m *identityManager) SwitchAccount(index uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeAccountIndex = index
	if len(m.cachedPrivKey) > 0 {
		memguard.WipeBytes(m.cachedPrivKey)
		m.cachedPrivKey = nil
	}
	m.cachedPubKey = nil
	m.cachedPeerID = types.PeerID{}
	if m.isUnlocked && m.KeyManager != nil {
		_ = m.refreshCachedKeys()
	}
}

func (m *identityManager) GetPublicKey() ([]byte, error) {
	m.mu.RLock()
	if m.isUnlocked && len(m.cachedPubKey) > 0 {
		defer m.mu.RUnlock()
		return append([]byte(nil), m.cachedPubKey...), nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isUnlocked || m.KeyManager == nil {
		return nil, ErrIdentityLocked
	}
	if len(m.cachedPubKey) > 0 {
		return append([]byte(nil), m.cachedPubKey...), nil
	}
	if err := m.refreshCachedKeys(); err != nil {
		return nil, err
	}
	return append([]byte(nil), m.cachedPubKey...), nil
}

type ProtectedMnemonic struct {
	mnemonic *memguard.LockedBuffer
	once     sync.Once
}

func (p *ProtectedMnemonic) Show() ([]byte, error) {
	if p.mnemonic == nil || p.mnemonic.Size() == 0 {
		return nil, errors.New("mnemonic has been destroyed or is empty")
	}

	bytes := p.mnemonic.Bytes()
	if len(bytes) == 0 {
		return nil, errors.New("mnemonic time-lock expired")
	}

	return bytes, nil
}

func (p *ProtectedMnemonic) Close() error {
	p.once.Do(func() {
		if p.mnemonic != nil {
			p.mnemonic.Destroy()
		}
	})
	return nil
}
