package identity

import (
	"bytes"
	"errors"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/storage"
)

type MockStorage struct {
	data map[string][]byte
}

func NewMockStorage() *MockStorage {
	return &MockStorage{
		data: make(map[string][]byte),
	}
}

func (m *MockStorage) Set(key, value []byte) error {
	m.data[string(key)] = append([]byte(nil), value...)
	return nil
}

func (m *MockStorage) Get(key []byte) ([]byte, error) {
	if val, ok := m.data[string(key)]; ok {
		return append([]byte(nil), val...), nil
	}
	return nil, errors.New("not found")
}

func (m *MockStorage) Delete(key []byte) error {
	delete(m.data, string(key))
	return nil
}

func (m *MockStorage) FindValuesSeek(prefix []byte, startKey []byte, limit int) ([]storage.Entry, error) {
	return nil, nil
}

func (m *MockStorage) FindValues(prefix []byte, limit int) ([]storage.Entry, error) {
	return nil, nil
}

func (m *MockStorage) Exists(key []byte) bool {
	_, ok := m.data[string(key)]
	return ok
}

func (m *MockStorage) Close() {}

func TestIdentityManager_Lifecycle(t *testing.T) {
	memguard.CatchInterrupt()
	defer memguard.Purge()

	st := NewMockStorage()
	im := newIdentityManager(st)

	mnemonic := []byte("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	passphrase := []byte("secure_password_123")

	err := im.RestoreFromMnemonic(append([]byte(nil), mnemonic...), append([]byte(nil), passphrase...))
	if err != nil {
		t.Fatalf("failed to restore: %v", err)
	}

	peerID1, err := im.GetOwnPeerID()
	if err != nil {
		t.Fatalf("failed to get peer ID: %v", err)
	}

	savePass := []byte("encryption_key_456")
	err = im.SaveToStorage(append([]byte(nil), savePass...))
	if err != nil {
		t.Fatalf("failed to save: %v", err)
	}

	im.PanicButton()

	im2 := newIdentityManager(st)
	err = im2.LoadFromStorage(append([]byte(nil), savePass...), false)
	if err != nil {
		t.Fatalf("failed to load: %v", err)
	}

	peerID2, err := im2.GetOwnPeerID()
	if err != nil {
		t.Fatalf("failed to get peer ID after load: %v", err)
	}

	if peerID1 != peerID2 {
		t.Fatalf("peer ID mismatch: %v != %v", peerID1, peerID2)
	}

	data := []byte("challenge_data")
	sig, err := im2.Sign(data)
	if err != nil || len(sig) == 0 {
		t.Fatalf("failed to sign data: %v", err)
	}
}

func TestIdentityManager_Security_WipeBytes(t *testing.T) {
	defer memguard.Purge()

	st := NewMockStorage()
	im := newIdentityManager(st)

	mnemonic := []byte("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	passphrase := []byte("secret_pass")

	mCopy := append([]byte(nil), mnemonic...)
	pCopy := append([]byte(nil), passphrase...)

	err := im.RestoreFromMnemonic(mCopy, pCopy)
	if err != nil {
		t.Fatalf("failed to restore: %v", err)
	}

	expectedZeroMnemonic := make([]byte, len(mnemonic))
	expectedZeroPassphrase := make([]byte, len(passphrase))

	if !bytes.Equal(mCopy, expectedZeroMnemonic) {
		t.Fatalf("CRITICAL SECURITY LEAK: mnemonic slice was not wiped")
	}

	if !bytes.Equal(pCopy, expectedZeroPassphrase) {
		t.Fatalf("CRITICAL SECURITY LEAK: passphrase slice was not wiped")
	}
}

func TestIdentityManager_Security_DuressFlow(t *testing.T) {
	defer memguard.Purge()

	st := NewMockStorage()
	im := newIdentityManager(st)

	mCopy := []byte("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	err := im.RestoreFromMnemonic(mCopy, []byte("real_pass"))
	if err != nil {
		t.Fatalf("failed to restore: %v", err)
	}

	savePass := []byte("storage_pass")
	err = im.SaveToStorage(append([]byte(nil), savePass...))
	if err != nil {
		t.Fatalf("failed to save: %v", err)
	}

	realPeerID, _ := im.GetOwnPeerID()

	imDuress := newIdentityManager(st)
	err = imDuress.LoadFromStorage(append([]byte(nil), savePass...), true)
	if err != nil {
		t.Fatalf("duress load failed: %v", err)
	}

	if !imDuress.isUnlocked {
		t.Fatalf("identity should appear unlocked during duress")
	}

	duressPeerID, err := imDuress.GetOwnPeerID()
	if err != nil {
		t.Fatalf("failed to get duress peer ID: %v", err)
	}

	if realPeerID == duressPeerID {
		t.Fatalf("CRITICAL: Duress flow yielded the real PeerID")
	}
}

func TestIdentityManager_Security_PanicButton(t *testing.T) {
	defer memguard.Purge()

	st := NewMockStorage()
	im := newIdentityManager(st)

	mCopy := []byte("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	err := im.RestoreFromMnemonic(mCopy, []byte(""))
	if err != nil {
		t.Fatalf("failed to restore: %v", err)
	}

	im.PanicButton()

	if im.isUnlocked {
		t.Fatalf("identity should be locked after panic")
	}

	if im.KeyManager != nil {
		t.Fatalf("CRITICAL: KeyManager was not destroyed during panic")
	}

	_, err = im.Sign([]byte("test"))
	if !errors.Is(err, ErrIdentityLocked) {
		t.Fatalf("expected ErrIdentityLocked, got: %v", err)
	}
}

func TestIdentityManager_Security_InvalidLoad(t *testing.T) {
	defer memguard.Purge()

	st := NewMockStorage()
	im := newIdentityManager(st)

	mCopy := []byte("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	_ = im.RestoreFromMnemonic(mCopy, []byte(""))

	_ = im.SaveToStorage([]byte("correct_password"))
	im.PanicButton()

	im2 := newIdentityManager(st)
	err := im2.LoadFromStorage([]byte("wrong_password"), false)

	if err == nil {
		t.Fatalf("CRITICAL: successfully loaded with wrong password")
	}

	if !errors.Is(err, ErrInvalidPass) {
		t.Fatalf("expected ErrInvalidPass, got: %v", err)
	}
}
