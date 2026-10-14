package keymanager

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/storage"
)

type mockStorage struct {
	data map[string][]byte
}

func (m *mockStorage) Get(key []byte) ([]byte, error) {
	if val, ok := m.data[string(key)]; ok {
		return append([]byte(nil), val...), nil
	}
	return nil, errors.New("not found")
}

func (m *mockStorage) Set(key []byte, value []byte) error {
	m.data[string(key)] = append([]byte(nil), value...)
	return nil
}

func (m *mockStorage) Delete(key []byte) error {
	delete(m.data, string(key))
	return nil
}

func (m *mockStorage) FindValues(prefix []byte, limit int) ([]storage.Entry, error) {
	var out []storage.Entry
	for k, v := range m.data {
		if bytes.HasPrefix([]byte(k), prefix) {
			out = append(out, storage.Entry{Key: []byte(k), Value: append([]byte(nil), v...)})
		}
	}
	return out, nil
}

func (m *mockStorage) FindValuesSeek(prefix []byte, startKey []byte, limit int) ([]storage.Entry, error) {
	return nil, nil
}

func (m *mockStorage) Exists(key []byte) bool {
	_, ok := m.data[string(key)]
	return ok
}

func (m *mockStorage) Close() {}

func TestKeyManager_FullSecureFlow(t *testing.T) {
	defer memguard.Purge()

	store := &mockStorage{data: make(map[string][]byte)}

	rawMnemonic := []byte("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	rawPassphrase := []byte("echo_duress_123")

	fmt.Println("==================================================")
	fmt.Printf("[ВИЗУАЛЬНЫЙ КОНТРОЛЬ] Входящая мнемоника: %s\n", string(rawMnemonic))
	fmt.Printf("[ВИЗУАЛЬНЫЙ КОНТРОЛЬ] Входящий пароль:    %s\n", string(rawPassphrase))
	fmt.Println("==================================================")

	mnemonicCopy := append([]byte(nil), rawMnemonic...)
	passphraseCopy := append([]byte(nil), rawPassphrase...)

	km, err := NewKeyManagerWithMnemonic(mnemonicCopy, passphraseCopy, store)
	if err != nil {
		t.Fatalf("Ошибка создания KeyManager: %v", err)
	}

	if !bytes.Equal(mnemonicCopy, make([]byte, len(rawMnemonic))) {
		t.Error("УЯЗВИМОСТЬ: мнемоника не была затерта в памяти после использования!")
	}

	account, err := km.GetAccount(0)
	if err != nil {
		t.Fatalf("Ошибка получения аккаунта: %v", err)
	}

	privEnc, pubKey, err := account.GetIdentityKey()
	if err != nil {
		t.Fatalf("Ошибка генерации Identity Key: %v", err)
	}

	lockedPriv, err := privEnc.Open()
	if err != nil {
		t.Fatalf("Не удалось вскрыть анклав ключа")
	}

	defer lockedPriv.Destroy()

	msg := []byte("EchoFog Secure Handshake Packet")

	tmpPriv := make([]byte, ed25519.PrivateKeySize)
	copy(tmpPriv, lockedPriv.Bytes())
	signature := ed25519.Sign(tmpPriv, msg)
	memguard.WipeBytes(tmpPriv)

	if !ed25519.Verify(pubKey, msg, signature) {
		t.Fatal("Криптография нарушена: подпись не прошла валидацию!")
	}

	fmt.Println("[УСПЕХ] Транзакция успешно подписана из защищенного анклава.")
}
