package selfcover

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/echofog/echofog-core/internal/v2/carrier"
)

// DefaultDirectory is the production ACME directory (Let's Encrypt). Nothing in this package
// contacts it unless the operator's configuration leaves ACMEConfig.DirectoryURL empty.
const DefaultDirectory = "https://acme-v02.api.letsencrypt.org/directory"

const (
	chalHTTP01    = "http-01"
	chalTLSALPN01 = "tls-alpn-01"

	// maxRenewBefore is how long before expiry a renewal starts when the certificate lives long
	// enough: the 30 days Let's Encrypt recommends.
	maxRenewBefore = 30 * 24 * time.Hour
	// retryAfterFailure is the base delay before a failed renewal is tried again.
	retryAfterFailure = time.Hour

	certKeyPrefix = "v2/reality2/self/cert/"
	acctKeyPrefix = "v2/reality2/self/acct/"
)

// ACMEConfig is the certificate side of the own cover. The domain is the operator's own, or a
// subdomain of a DDNS provider (DuckDNS and the like): the CA validates whatever name resolves
// to this node, so both work the same way.
type ACMEConfig struct {
	// DirectoryURL is the ACME directory; empty means DefaultDirectory.
	DirectoryURL string
	// Email is the optional contact of the account.
	Email string
	// AcceptTOS records that the operator accepted the CA's terms of service. Without it the
	// node does not register an account: accepting terms is the operator's act, not the code's.
	AcceptTOS bool
	// Challenge is "http-01", "tls-alpn-01" or empty: tls-alpn-01 when the CA offers it (it needs
	// only TCP 443), else http-01 (it needs TCP 80 too).
	Challenge string
	// HTTPClient is the client used for the CA; nil means a client with a 30 s timeout. Tests
	// point it at their own CA.
	HTTPClient *http.Client
	// RenewBefore is how long before expiry the certificate is renewed; zero means 30 days, or a
	// third of the certificate's lifetime when that is shorter.
	RenewBefore time.Duration
}

// CertsConfig configures Certs.
type CertsConfig struct {
	Domain string
	ACME   ACMEConfig
	// Static is a ready certificate for Domain (the operator's own cover_cert bundle). When set,
	// ACME is not used and the certificate is not renewed by this process: the operator renews
	// it and restarts the node. It must match Domain and its key.
	Static *tls.Certificate
	// KV keeps the account key and the certificate across restarts (T4.3); required, so a node
	// that restarts does not ask the CA for a new certificate each time and hit its rate limits.
	// It is still required with Static: the measured profiles and the replay cache live beside it.
	KV carrier.KV
	// Now and Sleep are the injectable clock of the renewal loop; nil means the real ones.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// OnChange is called after every certificate swap, with the new leaf. It runs on the
	// renewal goroutine and must not block for long.
	OnChange func(leaf *x509.Certificate)
	// OnError receives renewal errors; nil ignores them.
	OnError func(error)
}

// Certs obtains and renews the certificate of the cover by ACME and serves it to both the
// website and the admitted connections, so they present the very same certificate.
type Certs struct {
	cfg    CertsConfig
	domain string
	dir    string

	mu     sync.RWMutex
	cur    *tls.Certificate
	leaf   *x509.Certificate
	alpn   map[string]*tls.Certificate // tls-alpn-01 token certificates by server name
	tokens map[string]string           // http-01 key authorizations by token

	version atomic.Uint64
}

// NewCerts validates cfg.
func NewCerts(cfg CertsConfig) (*Certs, error) {
	d := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cfg.Domain), "."))
	if d == "" || strings.ContainsAny(d, " /:*") || !strings.Contains(d, ".") {
		return nil, fmt.Errorf("selfcover: %q is not a domain name", cfg.Domain)
	}
	if cfg.KV == nil {
		return nil, errors.New("selfcover: a key-value store is required for the certificate")
	}
	if cfg.ACME.Challenge != "" && cfg.ACME.Challenge != chalHTTP01 && cfg.ACME.Challenge != chalTLSALPN01 {
		return nil, fmt.Errorf("selfcover: unknown ACME challenge %q", cfg.ACME.Challenge)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	dir := cfg.ACME.DirectoryURL
	if dir == "" {
		dir = DefaultDirectory
	}
	c := &Certs{cfg: cfg, domain: d, dir: dir, alpn: map[string]*tls.Certificate{}, tokens: map[string]string{}}
	if cfg.Static != nil {
		leaf := cfg.Static.Leaf
		if leaf == nil {
			if len(cfg.Static.Certificate) == 0 {
				return nil, errors.New("selfcover: the static certificate has no leaf")
			}
			var err error
			if leaf, err = x509.ParseCertificate(cfg.Static.Certificate[0]); err != nil {
				return nil, fmt.Errorf("selfcover: static certificate: %w", err)
			}
			cfg.Static.Leaf = leaf
		}
		if err := leaf.VerifyHostname(d); err != nil {
			return nil, fmt.Errorf("selfcover: static certificate is not for %s: %w", d, err)
		}
		signer, ok := cfg.Static.PrivateKey.(crypto.Signer)
		if !ok || !samePublic(signer.Public(), leaf.PublicKey) {
			return nil, errors.New("selfcover: the static certificate key does not match")
		}
		c.cur, c.leaf = cfg.Static, leaf
	}
	return c, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Domain is the normalized name the certificate is for.
func (c *Certs) Domain() string { return c.domain }

// Version increases with every certificate swap.
func (c *Certs) Version() uint64 { return c.version.Load() }

// Leaf is the current leaf certificate, nil before the first one.
func (c *Certs) Leaf() *x509.Certificate {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.leaf
}

// Current is the certificate the website presents, nil before the first one.
func (c *Certs) Current() *tls.Certificate {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cur
}

// GetCertificate is the tls.Config hook of the website and of the admitted connections. It
// answers a tls-alpn-01 validation with the token certificate and everything else with the
// current certificate, whatever the SNI (a real single-site server does the same).
func (c *Certs) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == acme.ALPNProto {
		if cert := c.alpn[strings.ToLower(hello.ServerName)]; cert != nil {
			return cert, nil
		}
		return nil, errors.New("selfcover: no tls-alpn-01 challenge pending")
	}
	if c.cur == nil {
		return nil, errors.New("selfcover: no certificate yet")
	}
	return c.cur, nil
}

// HTTPHandler answers http-01 validation requests and hands everything else to fallback.
func (c *Certs) HTTPHandler(fallback http.Handler) http.Handler {
	const prefix = "/.well-known/acme-challenge/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, prefix) {
			c.mu.RLock()
			resp, ok := c.tokens[strings.TrimPrefix(r.URL.Path, prefix)]
			c.mu.RUnlock()
			if ok {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte(resp))
				return
			}
		}
		fallback.ServeHTTP(w, r)
	})
}

// Ensure makes sure a usable certificate is installed: the stored one when it is still good,
// otherwise a new one from the CA. The validation servers (HTTPHandler, the TLS listener) must
// already be running, because the CA connects to them. A static certificate is already installed.
func (c *Certs) Ensure(ctx context.Context) error {
	if c.cfg.Static != nil {
		return nil
	}
	if cert, err := c.load(ctx); err == nil && c.cfg.Now().Before(c.renewAt(cert.Leaf)) {
		c.install(cert)
		return nil
	}
	return c.renew(ctx)
}

// Run renews the certificate before it expires and returns when ctx ends. A failed renewal is
// retried after about an hour, and the certificate in use stays until a new one is installed.
// A static certificate is renewed by the operator, so Run returns at once.
func (c *Certs) Run(ctx context.Context) {
	if c.cfg.Static != nil {
		return
	}
	for {
		leaf := c.Leaf()
		wait := retryAfterFailure
		if leaf != nil {
			wait = c.renewAt(leaf).Sub(c.cfg.Now())
		}
		if wait < time.Minute {
			wait = time.Minute
		}
		if err := c.cfg.Sleep(ctx, wait); err != nil {
			return
		}
		if err := c.renew(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			if c.cfg.OnError != nil {
				c.cfg.OnError(fmt.Errorf("selfcover: renewing the certificate: %w", err))
			}
			// A short jitter keeps a fleet of nodes from retrying in step.
			if err := c.cfg.Sleep(ctx, retryAfterFailure+jitter(retryAfterFailure/2)); err != nil {
				return
			}
		}
	}
}

func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}

// renewAt is when the certificate should be renewed: RenewBefore (30 days, or a third of the
// lifetime of a short-lived certificate) before it expires.
func (c *Certs) renewAt(leaf *x509.Certificate) time.Time {
	before := c.cfg.ACME.RenewBefore
	if before <= 0 {
		before = maxRenewBefore
		if third := leaf.NotAfter.Sub(leaf.NotBefore) / 3; third < before {
			before = third
		}
	}
	return leaf.NotAfter.Add(-before)
}

func (c *Certs) install(cert *tls.Certificate) {
	c.mu.Lock()
	c.cur, c.leaf = cert, cert.Leaf
	c.mu.Unlock()
	c.version.Add(1)
	if c.cfg.OnChange != nil {
		c.cfg.OnChange(cert.Leaf)
	}
}

func (c *Certs) store(ctx context.Context, cert *tls.Certificate) error {
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	for _, der := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return c.cfg.KV.Put(ctx, []byte(certKeyPrefix+c.domain), out)
}

// load reads the stored certificate; it must be for this domain and not expired.
func (c *Certs) load(ctx context.Context) (*tls.Certificate, error) {
	b, ok, err := c.cfg.KV.Get(ctx, []byte(certKeyPrefix+c.domain))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("no stored certificate")
	}
	cert, err := parseBundle(b)
	if err != nil {
		return nil, err
	}
	if err := cert.Leaf.VerifyHostname(c.domain); err != nil {
		return nil, err
	}
	if !c.cfg.Now().Before(cert.Leaf.NotAfter) {
		return nil, errors.New("stored certificate expired")
	}
	return cert, nil
}

func parseBundle(b []byte) (*tls.Certificate, error) {
	var keyDER []byte
	var chain [][]byte
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		switch blk.Type {
		case "PRIVATE KEY":
			keyDER = blk.Bytes
		case "CERTIFICATE":
			chain = append(chain, blk.Bytes)
		}
	}
	if keyDER == nil || len(chain) == 0 {
		return nil, errors.New("incomplete certificate bundle")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("stored key cannot sign")
	}
	if !samePublic(signer.Public(), leaf.PublicKey) {
		return nil, errors.New("stored key does not match the certificate")
	}
	return &tls.Certificate{Certificate: chain, PrivateKey: signer, Leaf: leaf}, nil
}

func samePublic(a, b crypto.PublicKey) bool {
	type eq interface{ Equal(crypto.PublicKey) bool }
	k, ok := a.(eq)
	return ok && k.Equal(b)
}

// renew orders a new certificate and installs it.
func (c *Certs) renew(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cert, err := c.obtain(ctx)
	if err != nil {
		return err
	}
	if err := c.store(ctx, cert); err != nil {
		// A certificate that works but cannot be stored is still installed: the next start asks
		// the CA again, which is a cost, not an outage.
		if c.cfg.OnError != nil {
			c.cfg.OnError(fmt.Errorf("selfcover: storing the certificate: %w", err))
		}
	}
	c.install(cert)
	return nil
}

func (c *Certs) accountKey(ctx context.Context) (*ecdsa.PrivateKey, error) {
	sum := sha256.Sum256([]byte(c.dir))
	name := []byte(acctKeyPrefix + hex.EncodeToString(sum[:8]))
	if b, ok, err := c.cfg.KV.Get(ctx, name); err != nil {
		return nil, err
	} else if ok {
		if blk, _ := pem.Decode(b); blk != nil {
			if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
				return k, nil
			}
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := c.cfg.KV.Put(ctx, name, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})); err != nil {
		return nil, err
	}
	return k, nil
}

func (c *Certs) client(ctx context.Context) (*acme.Client, error) {
	if !c.cfg.ACME.AcceptTOS {
		return nil, errors.New("selfcover: the ACME terms of service are not accepted (ACMEConfig.AcceptTOS)")
	}
	key, err := c.accountKey(ctx)
	if err != nil {
		return nil, err
	}
	hc := c.cfg.ACME.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	cl := &acme.Client{Key: key, DirectoryURL: c.dir, HTTPClient: hc, UserAgent: "echofog-node"}
	acct := &acme.Account{}
	if c.cfg.ACME.Email != "" {
		acct.Contact = []string{"mailto:" + c.cfg.ACME.Email}
	}
	if _, err := cl.Register(ctx, acct, func(string) bool { return true }); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("registering the ACME account: %w", err)
	}
	return cl, nil
}

// obtain runs one ACME order for the domain and returns the issued certificate chain with its
// new ECDSA P-256 key: the key type of the cover, so its CertificateVerify has the usual size.
func (c *Certs) obtain(ctx context.Context) (*tls.Certificate, error) {
	cl, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	order, err := cl.AuthorizeOrder(ctx, acme.DomainIDs(c.domain))
	if err != nil {
		return nil, fmt.Errorf("creating the order: %w", err)
	}
	if order.Status != acme.StatusReady && order.Status != acme.StatusValid {
		for _, u := range order.AuthzURLs {
			if err := c.authorize(ctx, cl, u); err != nil {
				return nil, err
			}
		}
		if order, err = cl.WaitOrder(ctx, order.URI); err != nil {
			return nil, fmt.Errorf("waiting for the order: %w", err)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{c.domain}}, key)
	if err != nil {
		return nil, err
	}
	chain, _, err := cl.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("finalizing the order: %w", err)
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, err
	}
	if err := leaf.VerifyHostname(c.domain); err != nil || !samePublic(key.Public(), leaf.PublicKey) {
		return nil, errors.New("the CA issued a certificate that is not for this name and key")
	}
	return &tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}, nil
}

func (c *Certs) authorize(ctx context.Context, cl *acme.Client, authzURL string) error {
	a, err := cl.GetAuthorization(ctx, authzURL)
	if err != nil {
		return fmt.Errorf("reading the authorization: %w", err)
	}
	if a.Status == acme.StatusValid {
		return nil
	}
	chal := pickChallenge(a.Challenges, c.cfg.ACME.Challenge)
	if chal == nil {
		return fmt.Errorf("the CA offers no usable challenge (wanted %q)", c.cfg.ACME.Challenge)
	}
	cleanup, err := c.provision(cl, chal)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := cl.Accept(ctx, chal); err != nil {
		return fmt.Errorf("accepting the %s challenge: %w", chal.Type, err)
	}
	if _, err := cl.WaitAuthorization(ctx, a.URI); err != nil {
		return fmt.Errorf("the %s validation failed: %w", chal.Type, err)
	}
	return nil
}

// pickChallenge returns the wanted challenge; with no preference tls-alpn-01 comes first.
func pickChallenge(chs []*acme.Challenge, want string) *acme.Challenge {
	order := []string{chalTLSALPN01, chalHTTP01}
	if want != "" {
		order = []string{want}
	}
	for _, t := range order {
		for _, ch := range chs {
			if ch.Type == t {
				return ch
			}
		}
	}
	return nil
}

// provision makes the challenge answer available to the validation servers.
func (c *Certs) provision(cl *acme.Client, chal *acme.Challenge) (func(), error) {
	switch chal.Type {
	case chalHTTP01:
		resp, err := cl.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.tokens[chal.Token] = resp
		c.mu.Unlock()
		return func() { c.mu.Lock(); delete(c.tokens, chal.Token); c.mu.Unlock() }, nil
	case chalTLSALPN01:
		cert, err := cl.TLSALPN01ChallengeCert(chal.Token, c.domain)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.alpn[c.domain] = &cert
		c.mu.Unlock()
		return func() { c.mu.Lock(); delete(c.alpn, c.domain); c.mu.Unlock() }, nil
	}
	return nil, fmt.Errorf("unsupported challenge %q", chal.Type)
}
