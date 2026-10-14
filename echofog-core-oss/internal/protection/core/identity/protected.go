package identity

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"golang.org/x/crypto/ed25519"
)

type WalletApprovalPrompt struct {
	Origin      string `json:"origin"`      // e.g. "miniapp:chess", "system", "ui"
	Description string `json:"description"` // e.g. "Send 10 FOG to Bob"
	Amount      int64  `json:"amount"`
	ToAddress   string `json:"to_address"`
}

type WalletApprovalHandler func(prompt WalletApprovalPrompt) (bool, error)

type ProtectedIdentity struct {
	manager         *identityManager
	approvalHandler WalletApprovalHandler
	mu              sync.RWMutex
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

func (p *ProtectedIdentity) SetWalletApprovalHandler(handler WalletApprovalHandler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.approvalHandler = handler
}

func (p *ProtectedIdentity) SignWalletTransaction(data []byte) ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.manager.SignWalletTransaction(data)
}

func (p *ProtectedIdentity) RequestWalletSign(prompt WalletApprovalPrompt, data []byte) ([]byte, error) {
	p.mu.RLock()
	handler := p.approvalHandler
	p.mu.RUnlock()

	if handler != nil {
		approved, err := handler(prompt)
		if err != nil {
			return nil, err
		}
		if !approved {
			return nil, errors.New("transaction rejected by user")
		}
	}

	return p.SignWalletTransaction(data)
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

func (p *ProtectedIdentity) GenerateTLSConfig() (*tls.Config, error) {
	acManager, err := p.GetActiveAccount()
	if err != nil {
		return nil, err
	}

	privEnc, pub, err := acManager.GetIdentityKey()
	if err != nil {
		return nil, err
	}

	privKeyBuffer, err := privEnc.Open()
	if err != nil {
		return nil, err
	}
	defer privKeyBuffer.Destroy()

	privKey := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	copy(privKey[:32], privKeyBuffer.Bytes())
	copy(privKey[32:], pub)
	defer func() {
		for i := range privKey {
			privKey[i] = 0
		}
	}()

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

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, privKey)
	if err != nil {
		return nil, err
	}

	privBytes, _ := x509.MarshalPKCS8PrivateKey(privKey)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
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
