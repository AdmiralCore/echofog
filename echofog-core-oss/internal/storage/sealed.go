package storage

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"

	"github.com/awnumar/memguard"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// SealedPrefixes are the keys whose values are personal: messages, contacts, groups, the outbox,
// peers we talked to and their key bundles, mail we keep for others. Their values are stored encrypted.
var SealedPrefixes = [][]byte{
	[]byte("m:"),
	[]byte("outbox/"),
	[]byte("contacts:"),
	[]byte("peer:alias:"),
	[]byte("groups/g/"),
	[]byte("groups/m/"),
	[]byte("groups/k/"), // group keys: epochs and chains
	[]byte("groups/e/"), // sealed group messages kept to hand on
	[]byte("saved:peers:"),
	[]byte("mb:v1:"),
	[]byte("vpnmarket:"),
	[]byte("bcache:"),
	[]byte("fx:"),      // file transfers: names, paths and keys
	[]byte("bm:"),      // saved messages
	[]byte("blocked:"), // people the user blocked
	[]byte("msgreq:"),  // strangers who wrote first
}

var ErrSealed = errors.New("stored value cannot be opened")

// Blinder hides identifiers that would otherwise appear in database keys (who our contacts are,
// whom a queued message is for).
type Blinder interface {
	BlindID(id string) string
	BlindChat(chatID uint64) uint64
}

// BlindID returns id as st keeps it in keys: blinded when st is sealed, as it is otherwise.
func BlindID(st Storage, id string) string {
	if b, ok := st.(Blinder); ok {
		return b.BlindID(id)
	}
	return id
}

// BlindChat is BlindID for the chat number in message keys.
func BlindChat(st Storage, chatID uint64) uint64 {
	if b, ok := st.(Blinder); ok {
		return b.BlindChat(chatID)
	}
	return chatID
}

// Sealed encrypts the values under SealedPrefixes (XChaCha20-Poly1305, the database key as
// associated data, so a value cannot be moved under another key) and passes everything else
// through. Keys stay readable so that ordering and prefix scans keep working; identifiers in them
// are blinded by the services with BlindID / BlindChat.
type Sealed struct {
	inner    Storage
	sealKey  *memguard.Enclave
	blindKey *memguard.Enclave
}

// NewSealed wraps inner; secret is a 32-byte key that only the unlocked account can derive.
func NewSealed(inner Storage, secret *memguard.Enclave) (*Sealed, error) {
	s, err := secret.Open()
	if err != nil {
		return nil, err
	}
	defer s.Destroy()
	derive := func(label string) (*memguard.Enclave, error) {
		k := make([]byte, 32)
		if _, err := io.ReadFull(hkdf.New(sha256.New, s.Bytes(), nil, []byte(label)), k); err != nil {
			return nil, err
		}
		return memguard.NewEnclave(k), nil
	}
	seal, err := derive("FOG_DB_SEAL_V1")
	if err != nil {
		return nil, err
	}
	blind, err := derive("FOG_DB_BLIND_V1")
	if err != nil {
		return nil, err
	}
	return &Sealed{inner: inner, sealKey: seal, blindKey: blind}, nil
}

// Inner is the storage underneath, for the migration.
func (s *Sealed) Inner() Storage { return s.inner }

func isSealed(key []byte) bool {
	for _, p := range SealedPrefixes {
		if bytes.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func (s *Sealed) aead() (interface {
	Seal(dst, nonce, plaintext, ad []byte) []byte
	Open(dst, nonce, ciphertext, ad []byte) ([]byte, error)
	NonceSize() int
	Overhead() int
}, func(), error) {
	k, err := s.sealKey.Open()
	if err != nil {
		return nil, nil, err
	}
	a, err := chacha20poly1305.NewX(k.Bytes())
	if err != nil {
		k.Destroy()
		return nil, nil, err
	}
	return a, k.Destroy, nil
}

func (s *Sealed) seal(key, value []byte) ([]byte, error) {
	a, done, err := s.aead()
	if err != nil {
		return nil, err
	}
	defer done()
	nonce := make([]byte, a.NonceSize(), a.NonceSize()+len(value)+a.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return a.Seal(nonce, nonce, value, key), nil
}

func (s *Sealed) open(key, value []byte) ([]byte, error) {
	a, done, err := s.aead()
	if err != nil {
		return nil, err
	}
	defer done()
	if len(value) < a.NonceSize()+a.Overhead() {
		return nil, ErrSealed
	}
	out, err := a.Open(nil, value[:a.NonceSize()], value[a.NonceSize():], key)
	if err != nil {
		return nil, ErrSealed
	}
	return out, nil
}

// Opens tells whether the value stored under key is sealed with our key.
func (s *Sealed) Opens(key, value []byte) bool {
	_, err := s.open(key, value)
	return err == nil
}

func (s *Sealed) Set(key, value []byte) error {
	if !isSealed(key) {
		return s.inner.Set(key, value)
	}
	sealed, err := s.seal(key, value)
	if err != nil {
		return err
	}
	return s.inner.Set(key, sealed)
}

func (s *Sealed) Get(key []byte) ([]byte, error) {
	v, err := s.inner.Get(key)
	if err != nil || !isSealed(key) {
		return v, err
	}
	return s.open(key, v)
}

func (s *Sealed) Delete(key []byte) error { return s.inner.Delete(key) }

func (s *Sealed) Exists(key []byte) bool { return s.inner.Exists(key) }

func (s *Sealed) Close() { s.inner.Close() }

// openAll opens the sealed values of entries; values that do not open (written by somebody else,
// damaged) are left out.
func (s *Sealed) openAll(entries []Entry) []Entry {
	out := entries[:0]
	for _, e := range entries {
		if isSealed(e.Key) {
			v, err := s.open(e.Key, e.Value)
			if err != nil {
				continue
			}
			e.Value = v
		}
		out = append(out, e)
	}
	return out
}

func (s *Sealed) FindValues(prefix []byte, limit int) ([]Entry, error) {
	entries, err := s.inner.FindValues(prefix, limit)
	if err != nil {
		return nil, err
	}
	return s.openAll(entries), nil
}

func (s *Sealed) FindValuesSeek(prefix []byte, startKey []byte, limit int) ([]Entry, error) {
	entries, err := s.inner.FindValuesSeek(prefix, startKey, limit)
	if err != nil {
		return nil, err
	}
	return s.openAll(entries), nil
}

// GetNextID passes the message counter through (it holds no content).
func (s *Sealed) GetNextID(chatID uint64) (uint64, error) {
	ms, ok := s.inner.(MessageStorage)
	if !ok {
		return 0, errors.New("storage has no message counters")
	}
	return ms.GetNextID(chatID)
}

func (s *Sealed) mac(data []byte) []byte {
	k, err := s.blindKey.Open()
	if err != nil {
		return nil
	}
	defer k.Destroy()
	h := hmac.New(sha256.New, k.Bytes())
	h.Write(data)
	return h.Sum(nil)
}

func (s *Sealed) BlindID(id string) string {
	return hex.EncodeToString(s.mac(append([]byte("id:"), id...))[:16])
}

func (s *Sealed) BlindChat(chatID uint64) uint64 {
	b := binary.BigEndian.AppendUint64([]byte("chat:"), chatID)
	return binary.BigEndian.Uint64(s.mac(b)[:8])
}
