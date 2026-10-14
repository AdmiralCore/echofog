package testutil

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// GetRandomKeyAndTls returns a fresh identity key and a TLS config with a self-signed certificate
// for it, shaped like the one a real node presents (peers authenticate each other at the
// application layer, so the certificate itself is not verified).
func GetRandomKeyAndTls(t *testing.T) (types.PeerPrivateKey, *tls.Config) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tlsCfg, err := selfSignedTLS(key)
	if err != nil {
		t.Fatal(err)
	}

	privKey, err := types.ToPrivKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return privKey, tlsCfg
}

func selfSignedTLS(priv ed25519.PrivateKey) (*tls.Config, error) {
	pub := priv.Public().(ed25519.PublicKey)

	template := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: hex.EncodeToString(pub), Organization: []string{"EchoFog"}},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, priv)
	if err != nil {
		return nil, err
	}
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	tlsCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}),
	)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates:       []tls.Certificate{tlsCert},
		NextProtos:         []string{"echo-fog-protocole"},
		ClientAuth:         tls.RequestClientCert,
		InsecureSkipVerify: true, // #nosec G402 -- test certificates; peers prove themselves in the handshake
		VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error { // #nosec G123
			return nil
		},
		ClientSessionCache: tls.NewLRUClientSessionCache(100),
	}, nil
}
