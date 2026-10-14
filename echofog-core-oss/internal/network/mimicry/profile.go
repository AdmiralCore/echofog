package mimicry

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
)

// BrowserProfile represents a browser TLS fingerprint configuration.
type BrowserProfile string

const (
	ProfileChrome  BrowserProfile = "chrome"
	ProfileFirefox BrowserProfile = "firefox"
	ProfileSafari  BrowserProfile = "safari"
)

// MimicryConfig configures TLS client and server masquerading.
type MimicryConfig struct {
	// ServerName (SNI) to mimic on the wire (e.g. "www.microsoft.com", "gateway.icloud.com")
	ServerName string `json:"server_name"`
	// Profile determines the TLS fingerprint cipher suites and curve preferences
	Profile BrowserProfile `json:"profile"`
	// AuthKey is the 32-byte shared secret for REALITY-style stealth authentication
	AuthKey []byte `json:"auth_key"`
	// FallbackAddr is the external legitimate address to proxy active probes to (e.g. "www.microsoft.com:443")
	FallbackAddr string `json:"fallback_addr"`
	// InsecureSkipVerify allows testing with self-signed test certificates
	InsecureSkipVerify bool `json:"insecure_skip_verify"`
}

// DefaultStealthKey derives the fallback stealth-token key when no password is configured.
// It is domain-separated rather than a raw key literal, but it is still public in the source,
// so real deployments must set anti_dpi.stealth_password (see StealthKeyFromPassword).
func DefaultStealthKey() []byte {
	sum := sha256.Sum256([]byte("echofog/tcp-stealth/default-network/v1"))
	return sum[:]
}

// NodeStealthKey derives the stealth-token key of one node from its peer ID: a client that dials
// a node knows its ID and computes the key, a scanner does not. secret is an optional extra secret
// for a private network. No key is shared by the whole network.
func NodeStealthKey(secret string, id [32]byte) []byte {
	h := sha256.New()
	h.Write([]byte("echofog/tcp-stealth/node/v2\x00"))
	h.Write([]byte(secret))
	h.Write([]byte{0})
	h.Write(id[:])
	return h.Sum(nil)
}

// StealthKeyFromPassword derives the stealth-token key from a network secret.
func StealthKeyFromPassword(password string) []byte {
	if password == "" {
		return DefaultStealthKey()
	}
	sum := sha256.Sum256([]byte("echofog/tcp-stealth/" + password))
	return sum[:]
}

// DefaultMimicryConfig returns a battle-tested browser mimicry configuration.
func DefaultMimicryConfig() *MimicryConfig {
	return &MimicryConfig{
		ServerName:   "www.microsoft.com",
		Profile:      ProfileChrome,
		AuthKey:      DefaultStealthKey(),
		FallbackAddr: "",
	}
}

// BuildServerTLSConfig constructs a TLS server config supporting standard TLS 1.3.
func BuildServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},
		// X25519MLKEM768 first: modern Chrome offers the post-quantum hybrid and big sites
		// negotiate it, so the key_share group in our ServerHello matches theirs (a JA3S input).
		CurvePreferences: []tls.CurveID{
			tls.X25519MLKEM768,
			tls.X25519,
			tls.CurveP256,
		},
		NextProtos: []string{"h2", "http/1.1"},
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error { // #nosec G123 -- no certificate is trusted here, see the stealth token
			// Transparently accept peer certificate; identity authentication is handled by AEAD stealth token
			return nil
		},
	}
}
