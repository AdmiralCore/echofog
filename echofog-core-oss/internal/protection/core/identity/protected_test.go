package identity

import (
	"crypto"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/awnumar/memguard"
)

const testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

func unlockedIdentity(t *testing.T) *ProtectedIdentity {
	t.Helper()
	p := NewProtectedIdentity(NewMockStorage())
	if err := p.Restore([]byte(testMnemonic), []byte("")); err != nil {
		t.Fatalf("restore identity: %v", err)
	}
	return p
}

// The identity TLS certificate must complete a real handshake even though its private key never
// leaves the memguard enclave as plaintext.
func TestGenerateTLSConfig_Handshake(t *testing.T) {
	defer memguard.Purge()
	p := unlockedIdentity(t)

	serverCfg, err := p.GenerateTLSConfig()
	if err != nil {
		t.Fatalf("server tls config: %v", err)
	}
	clientCfg, err := p.GenerateTLSConfig()
	if err != nil {
		t.Fatalf("client tls config: %v", err)
	}

	// The cert's private key is a signer, not a raw key: no plaintext ed25519 key sits in the config.
	if _, ok := serverCfg.Certificates[0].PrivateKey.(crypto.Signer); !ok {
		t.Fatalf("tls certificate private key is not a crypto.Signer: %T", serverCfg.Certificates[0].PrivateKey)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	errc := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errc <- err
			return
		}
		defer conn.Close()
		server := tls.Server(conn, serverCfg)
		_ = server.SetDeadline(time.Now().Add(10 * time.Second))
		errc <- server.Handshake()
	}()

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 10*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := tls.Client(conn, clientCfg)
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
}

// After the panic button, the enclave-backed signer can no longer produce a signature: the key is
// genuinely gone from memory, unlike a tls.Certificate that would keep the raw key on the heap.
func TestGenerateTLSConfig_KeyWipedOnPanic(t *testing.T) {
	defer memguard.Purge()
	p := unlockedIdentity(t)

	cfg, err := p.GenerateTLSConfig()
	if err != nil {
		t.Fatalf("tls config: %v", err)
	}
	signer := cfg.Certificates[0].PrivateKey.(crypto.Signer)

	// Sanity: it signs while unlocked.
	if _, err := signer.Sign(nil, []byte("hello echofog"), crypto.Hash(0)); err != nil {
		t.Fatalf("signer should work before panic: %v", err)
	}

	p.ActivatePanicButton()

	if _, err := signer.Sign(nil, []byte("hello echofog"), crypto.Hash(0)); err == nil {
		t.Fatal("signer still produced a signature after the panic button; the TLS key was not wiped")
	}
}
