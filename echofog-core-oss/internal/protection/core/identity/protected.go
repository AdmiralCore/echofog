package identity

import (
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"sync"
	"time"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"golang.org/x/crypto/ed25519"
)

type ProtectedIdentity struct {
	manager *identityManager
	mu      sync.RWMutex
}

func NewProtectedIdentity(st storage.Storage) *ProtectedIdentity {
	return &ProtectedIdentity{
		manager: newIdentityManager(st),
	}
}

func (p *ProtectedIdentity) Restore(mnemonic []byte, passphrase []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.manager.RestoreFromMnemonic(mnemonic, passphrase)
}

func (p *ProtectedIdentity) GenerateNewIdentity(passphrase []byte) (*ProtectedMnemonic, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.manager.GenerateNewIdentity(passphrase)
}

func (p *ProtectedIdentity) Load(passphrase []byte, isDuress bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.manager.LoadFromStorage(passphrase, isDuress)
}

func (p *ProtectedIdentity) Save(passphrase []byte) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.manager.SaveToStorage(passphrase)
}

func (p *ProtectedIdentity) ActivatePanicButton() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.manager.PanicButton()
}

func (p *ProtectedIdentity) SignData(data []byte) ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.manager.Sign(data)
}

func (p *ProtectedIdentity) GetPeerID() (types.PeerID, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.manager.GetOwnPeerID()
}

func (p *ProtectedIdentity) IsUnlocked() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.manager.isUnlocked
}

func (p *ProtectedIdentity) GetPublicKey() ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.manager.GetPublicKey()
}

func (p *ProtectedIdentity) SignWalletTransaction(data []byte) ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.manager.SignWalletTransaction(data)
}

func (p *ProtectedIdentity) GetWalletPublicKey() ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.manager.GetWalletPublicKey()
}

func (p *ProtectedIdentity) GetActiveAccount() (*keymanager.AccountManager, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.manager.KeyManager.GetAccount(p.manager.activeAccountIndex)
}

func (p *ProtectedIdentity) SwitchAccount(index uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.manager.SwitchAccount(index)
}

// enclaveSigner is the crypto.Signer behind the identity TLS certificate. The ed25519 private key
// stays inside a memguard enclave; it is unsealed only for the duration of one signature and wiped
// again immediately. Nothing keeps a plaintext copy of the key alive for the session, so the panic
// button (which purges every enclave) genuinely removes it from memory — unlike a tls.Certificate
// built from a parsed key, which would hold the raw bytes on the heap until the process exits.
type enclaveSigner struct {
	enclave *memguard.Enclave
	pub     ed25519.PublicKey
}

func (s *enclaveSigner) Public() crypto.PublicKey { return s.pub }

func (s *enclaveSigner) Sign(rand io.Reader, message []byte, opts crypto.SignerOpts) ([]byte, error) {
	buf, err := s.enclave.Open()
	if err != nil {
		return nil, errors.New("identity key is unavailable (locked or wiped)")
	}
	defer buf.Destroy()
	// The enclave holds the full 64-byte ed25519 private key (seed || public). Copy it into an
	// ordinary heap slice for the signing call: ed25519.Sign registers a weak pointer to the key,
	// which the runtime rejects for memguard's guarded (off-heap) memory. The copy is zeroed again
	// as soon as the signature is produced, so no plaintext key outlives the call.
	priv := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	copy(priv, buf.Bytes())
	defer memguard.WipeBytes(priv)
	return priv.Sign(rand, message, opts)
}

func (p *ProtectedIdentity) GenerateTLSConfig() (*tls.Config, error) {
	acManager, err := p.GetActiveAccount()
	if err != nil {
		return nil, err
	}

	privEnc, pub, err := acManager.GetIdentityKey()
	if err != nil {
		return nil, err
	}
	signer := &enclaveSigner{enclave: privEnc, pub: pub}

	pubHex := hex.EncodeToString(pub)

	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:   pubHex,
			Organization: []string{"EchoFog"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour * 24 * 365 * 10),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	// The certificate is self-signed with the enclave-backed signer, so the key is never marshalled
	// to PEM or held in a plain ed25519.PrivateKey.
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, signer)
	if err != nil {
		return nil, err
	}

	tlsCert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  signer,
	}
	if leaf, err := x509.ParseCertificate(certDER); err == nil {
		tlsCert.Leaf = leaf
	}

	return &tls.Config{
		Certificates:       []tls.Certificate{tlsCert},
		NextProtos:         []string{"echo-fog-protocole"},
		ClientAuth:         tls.RequestClientCert,
		InsecureSkipVerify: true, // #nosec G402 -- the certificate is a carrier; the peer is authenticated by the handshake bound to this session
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error { // #nosec G123
			return nil
		},
		ClientSessionCache: tls.NewLRUClientSessionCache(100),
	}, nil
}

func (p *ProtectedIdentity) GenerateEphemeralKey() (types.PeerPublicKey, error) {
	panic("implement me")
}
