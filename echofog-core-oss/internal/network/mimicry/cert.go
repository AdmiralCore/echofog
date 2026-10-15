package mimicry

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"io"
	"math/big"
	"time"
)

// fillerOID is a private, non-critical extension used only to pad the certificate's encoded size.
// It carries random bytes and means nothing; it exists so our first-flight TLS Certificate record
// is not a tiny fixed size (see padTargetMin/Max).
var fillerOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 58888, 1}

const (
	// A self-signed single certificate encodes to a few hundred bytes, while a real site serves a
	// chain of several kilobytes. In TLS 1.3 the Certificate message is encrypted, so a passive
	// observer cannot read it, but it can still measure the record's length. A node that always
	// emitted a tiny Certificate record would stand out from the site it mimics on exactly this
	// length. We pad the certificate to a size drawn from a realistic range, randomised per node so
	// there is no fixed "EchoFog certificate size" across the network either. An active prober never
	// reaches this: it is forwarded to the real site before our TLS begins.
	padTargetMin = 2800
	padTargetMax = 5200
)

// GenerateCamouflageCert makes a self-signed ECDSA P-256 certificate for serverName, padded to a
// realistic encoded size. Google Chrome's TLS ClientHello does not offer ed25519 signature
// algorithms, so the fallback must present an ECDSA (or RSA) certificate; the node's ed25519
// identity cert cannot be reused here. This certificate is self-signed, so it resists passive
// fingerprinting (including the first-flight length, thanks to the padding) but not an active
// prober checking the chain; configure a real certificate for your domain, or let a public node
// adopt a neighbour's domain, for that.
func GenerateCamouflageCert(serverName string) (tls.Certificate, error) {
	if serverName == "" {
		serverName = "www.microsoft.com"
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	newTemplate := func() *x509.Certificate {
		return &x509.Certificate{
			SerialNumber:          serial,
			Subject:               pkix.Name{CommonName: serverName},
			DNSNames:              []string{serverName},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(825 * 24 * time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
		}
	}

	// First pass measures the unpadded size, then we add exactly enough random filler to reach a
	// random target length in [padTargetMin, padTargetMax].
	base, err := x509.CreateCertificate(rand.Reader, newTemplate(), newTemplate(), &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	target, err := randIntBetween(padTargetMin, padTargetMax)
	if err != nil {
		return tls.Certificate{}, err
	}
	fillerLen := target - len(base)
	if fillerLen < 1 {
		fillerLen = 1 // the certificate is already larger than the target; add a token amount
	}
	filler := make([]byte, fillerLen)
	if _, err := io.ReadFull(rand.Reader, filler); err != nil {
		return tls.Certificate{}, err
	}
	tpl := newTemplate()
	tpl.ExtraExtensions = []pkix.Extension{{Id: fillerOID, Critical: false, Value: filler}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// randIntBetween returns a uniform integer in [min, max].
func randIntBetween(min, max int) (int, error) {
	if max <= min {
		return min, nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max-min+1)))
	if err != nil {
		return 0, err
	}
	return min + int(n.Int64()), nil
}
