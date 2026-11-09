package servernode_test

// A minimal in-process ACME server (RFC 8555) for the tests: accounts, orders and tls-alpn-01
// validation by really connecting to the node, then issuance from its own CA. No test contacts a
// real CA.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeACME struct {
	srv    *httptest.Server
	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
	caDER  []byte
	// resolve returns the TCP address a tls-alpn-01 validation connects to for a domain.
	resolve func(domain string) string

	mu      sync.Mutex
	nonce   int
	acct    map[string]string
	orders  map[string]*acmeOrder
	authzs  map[string]*acmeAuthz
	counter int
	certs   map[string][]byte
}

type acmeOrder struct {
	id, domain, authz, status, certID string
}

type acmeAuthz struct {
	id, domain, status, token, thumb string
}

func newFakeACME(t *testing.T) *fakeACME {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	ca := &fakeACME{
		caKey: key, caCert: cert, caDER: der,
		acct: map[string]string{}, orders: map[string]*acmeOrder{}, authzs: map[string]*acmeAuthz{}, certs: map[string][]byte{},
	}
	ca.srv = httptest.NewServer(http.HandlerFunc(ca.serve))
	t.Cleanup(ca.srv.Close)
	return ca
}

func (ca *fakeACME) URL() string { return ca.srv.URL + "/dir" }

func (ca *fakeACME) nextNonce() string {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.nonce++
	return "nonce-" + strconv.Itoa(ca.nonce)
}

func (ca *fakeACME) reply(w http.ResponseWriter, code int, v any, hdr map[string]string) {
	w.Header().Set("Replay-Nonce", ca.nextNonce())
	for k, val := range hdr {
		w.Header().Set(k, val)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type acmeJWS struct {
	Protected string `json:"protected"`
	Payload   string `json:"payload"`
}

type acmeJWSHeader struct {
	URL string `json:"url"`
	Kid string `json:"kid"`
	JWK *struct {
		Kty, Crv, X, Y string
	} `json:"jwk"`
}

func acmeThumbprint(h *acmeJWSHeader) string {
	if h.JWK == nil {
		return ""
	}
	j := fmt.Sprintf(`{"crv":%q,"kty":%q,"x":%q,"y":%q}`, h.JWK.Crv, h.JWK.Kty, h.JWK.X, h.JWK.Y)
	sum := sha256.Sum256([]byte(j))
	return b64url(sum[:])
}

func (ca *fakeACME) count() int {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.counter++
	return ca.counter
}

func (ca *fakeACME) serve(w http.ResponseWriter, r *http.Request) {
	base := ca.srv.URL
	switch r.URL.Path {
	case "/dir":
		ca.reply(w, 200, map[string]any{
			"newNonce": base + "/nonce", "newAccount": base + "/acct", "newOrder": base + "/order",
			"revokeCert": base + "/revoke", "keyChange": base + "/key",
		}, nil)
		return
	case "/nonce":
		ca.reply(w, 200, nil, nil)
		return
	}
	var env acmeJWS
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &env); err != nil {
		ca.reply(w, 400, map[string]any{"type": "urn:ietf:params:acme:error:malformed"}, nil)
		return
	}
	var hdr acmeJWSHeader
	raw, _ := base64.RawURLEncoding.DecodeString(env.Protected)
	_ = json.Unmarshal(raw, &hdr)
	payload, _ := base64.RawURLEncoding.DecodeString(env.Payload)

	thumbOf := func() string {
		ca.mu.Lock()
		defer ca.mu.Unlock()
		return ca.acct[hdr.Kid]
	}
	path := r.URL.Path
	switch {
	case path == "/acct":
		id := "/acct/" + strconv.Itoa(ca.count())
		ca.mu.Lock()
		ca.acct[base+id] = acmeThumbprint(&hdr)
		ca.mu.Unlock()
		ca.reply(w, 201, map[string]any{"status": "valid"}, map[string]string{"Location": base + id})
	case path == "/order":
		var req struct {
			Identifiers []struct{ Type, Value string } `json:"identifiers"`
		}
		_ = json.Unmarshal(payload, &req)
		if len(req.Identifiers) != 1 {
			ca.reply(w, 400, map[string]any{"type": "urn:ietf:params:acme:error:malformed"}, nil)
			return
		}
		n := ca.count()
		az := &acmeAuthz{id: strconv.Itoa(n), domain: req.Identifiers[0].Value, status: "pending",
			token: b64url([]byte("alpn-token-" + strconv.Itoa(n))), thumb: thumbOf()}
		o := &acmeOrder{id: strconv.Itoa(n), domain: az.domain, authz: az.id, status: "pending"}
		ca.mu.Lock()
		ca.authzs[az.id], ca.orders[o.id] = az, o
		ca.mu.Unlock()
		ca.reply(w, 201, ca.orderJSON(o), map[string]string{"Location": base + "/order/" + o.id})
	case strings.HasPrefix(path, "/authz/"):
		ca.mu.Lock()
		az := ca.authzs[strings.TrimPrefix(path, "/authz/")]
		ca.mu.Unlock()
		if az == nil {
			ca.reply(w, 404, map[string]any{"type": "urn:ietf:params:acme:error:malformed"}, nil)
			return
		}
		ca.reply(w, 200, ca.authzJSON(az), nil)
	case strings.HasPrefix(path, "/chal/"):
		parts := strings.Split(strings.TrimPrefix(path, "/chal/"), "/")
		ca.mu.Lock()
		var az *acmeAuthz
		if len(parts) == 2 {
			az = ca.authzs[parts[0]]
		}
		ca.mu.Unlock()
		if az == nil {
			ca.reply(w, 404, nil, nil)
			return
		}
		ok := ca.validate(az, parts[1])
		ca.mu.Lock()
		if ok {
			az.status = "valid"
		} else {
			az.status = "invalid"
		}
		ca.mu.Unlock()
		ca.reply(w, 200, map[string]any{"type": parts[1], "url": base + path, "status": "processing"}, nil)
	case strings.HasPrefix(path, "/order/"):
		ca.mu.Lock()
		o := ca.orders[strings.TrimPrefix(path, "/order/")]
		if o != nil && o.status == "pending" && ca.authzs[o.authz].status == "valid" {
			o.status = "ready"
		}
		ca.mu.Unlock()
		if o == nil {
			ca.reply(w, 404, nil, nil)
			return
		}
		ca.reply(w, 200, ca.orderJSON(o), nil)
	case strings.HasPrefix(path, "/finalize/"):
		ca.mu.Lock()
		o := ca.orders[strings.TrimPrefix(path, "/finalize/")]
		ca.mu.Unlock()
		var req struct{ CSR string }
		_ = json.Unmarshal(payload, &req)
		der, _ := base64.RawURLEncoding.DecodeString(req.CSR)
		csr, err := x509.ParseCertificateRequest(der)
		if err != nil || o == nil || csr.CheckSignature() != nil {
			ca.reply(w, 400, map[string]any{"type": "urn:ietf:params:acme:error:badCSR"}, nil)
			return
		}
		chain := ca.issue(csr, o.domain)
		ca.mu.Lock()
		o.status, o.certID = "valid", o.id
		ca.certs[o.id] = chain
		ca.mu.Unlock()
		ca.reply(w, 200, ca.orderJSON(o), nil)
	case strings.HasPrefix(path, "/cert/"):
		ca.mu.Lock()
		c := ca.certs[strings.TrimPrefix(path, "/cert/")]
		ca.mu.Unlock()
		w.Header().Set("Replay-Nonce", ca.nextNonce())
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_, _ = w.Write(c)
	default:
		ca.reply(w, 404, nil, nil)
	}
}

func (ca *fakeACME) orderJSON(o *acmeOrder) map[string]any {
	base := ca.srv.URL
	ca.mu.Lock()
	defer ca.mu.Unlock()
	m := map[string]any{
		"status":         o.status,
		"identifiers":    []map[string]string{{"type": "dns", "value": o.domain}},
		"authorizations": []string{base + "/authz/" + o.authz},
		"finalize":       base + "/finalize/" + o.id,
	}
	if o.certID != "" {
		m["certificate"] = base + "/cert/" + o.certID
	}
	return m
}

func (ca *fakeACME) authzJSON(az *acmeAuthz) map[string]any {
	base := ca.srv.URL
	ca.mu.Lock()
	st := az.status
	ca.mu.Unlock()
	return map[string]any{
		"status": st, "identifier": map[string]string{"type": "dns", "value": az.domain},
		"challenges": []map[string]any{
			{"type": "tls-alpn-01", "url": base + "/chal/" + az.id + "/tls-alpn-01", "token": az.token, "status": "pending"},
			{"type": "http-01", "url": base + "/chal/" + az.id + "/http-01", "token": az.token, "status": "pending"},
		},
	}
}

func (ca *fakeACME) validate(az *acmeAuthz, typ string) bool {
	if typ != "tls-alpn-01" {
		return false
	}
	addr := ca.resolve(az.domain)
	d := &tls.Dialer{NetDialer: nil, Config: &tls.Config{
		ServerName: az.domain, NextProtos: []string{"acme-tls/1"}, InsecureSkipVerify: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer c.Close()
	st := c.(*tls.Conn).ConnectionState()
	if st.NegotiatedProtocol != "acme-tls/1" || len(st.PeerCertificates) != 1 {
		return false
	}
	leaf := st.PeerCertificates[0]
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != az.domain {
		return false
	}
	sum := sha256.Sum256([]byte(az.token + "." + az.thumb))
	want, _ := asn1.Marshal(sum[:])
	for _, e := range leaf.Extensions {
		if e.Id.Equal(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 31}) {
			return string(e.Value) == string(want)
		}
	}
	return false
}

func (ca *fakeACME) issue(csr *x509.CertificateRequest, domain string) []byte {
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	tpl := &x509.Certificate{
		SerialNumber: serial, DNSNames: []string{domain},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.caCert, csr.PublicKey, ca.caKey)
	if err != nil {
		panic(err)
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.caDER})...)
}
