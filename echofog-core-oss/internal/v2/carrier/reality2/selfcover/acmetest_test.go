package selfcover

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
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCA is a small in-process ACME server (RFC 8555) for the tests: accounts, orders,
// http-01 and tls-alpn-01 validation by really connecting to the node, and issuance of
// certificates from its own CA. It never talks to the network beyond the addresses Resolve
// returns, so no test sends a request to a real CA. It does not verify JWS signatures.
type fakeCA struct {
	t        *testing.T
	srv      *httptest.Server
	caKey    *ecdsa.PrivateKey
	caCert   *x509.Certificate
	caDER    []byte
	validity time.Duration

	// Resolve returns where the validation of domain connects: the http-01 and tls-alpn-01 addresses.
	Resolve func(domain string) (httpAddr, tlsAddr string)

	mu      sync.Mutex
	nonce   int
	acct    map[string]string // account url -> thumbprint
	orders  map[string]*caOrder
	authzs  map[string]*caAuthz
	issued  int
	certs   map[string][]byte
	counter int
	// validated counts successful validations by challenge type.
	validated map[string]int
}

type caOrder struct {
	id       string
	domain   string
	authz    string
	status   string
	certID   string
	acctThmb string
}

type caAuthz struct {
	id       string
	domain   string
	status   string
	httpTok  string
	alpnTok  string
	acctThmb string
}

func newFakeCA(t *testing.T) *fakeCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	ca := &fakeCA{t: t, caKey: key, caCert: cert, caDER: der, validity: 90 * 24 * time.Hour,
		acct: map[string]string{}, orders: map[string]*caOrder{}, authzs: map[string]*caAuthz{},
		certs: map[string][]byte{}, validated: map[string]int{}}
	ca.srv = httptest.NewServer(http.HandlerFunc(ca.serve))
	t.Cleanup(ca.srv.Close)
	return ca
}

func (ca *fakeCA) URL() string { return ca.srv.URL + "/dir" }

func (ca *fakeCA) Issued() int { ca.mu.Lock(); defer ca.mu.Unlock(); return ca.issued }

func (ca *fakeCA) Validated(typ string) int {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return ca.validated[typ]
}

func (ca *fakeCA) SetValidity(d time.Duration) { ca.mu.Lock(); ca.validity = d; ca.mu.Unlock() }

func (ca *fakeCA) nextNonce() string {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.nonce++
	return "nonce-" + strconv.Itoa(ca.nonce)
}

func (ca *fakeCA) reply(w http.ResponseWriter, code int, v any, hdr map[string]string) {
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

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type jws struct {
	Protected string `json:"protected"`
	Payload   string `json:"payload"`
}

type jwsHeader struct {
	URL string `json:"url"`
	Kid string `json:"kid"`
	JWK *struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
	} `json:"jwk"`
}

func thumbprint(h *jwsHeader) string {
	if h.JWK == nil {
		return ""
	}
	j := fmt.Sprintf(`{"crv":%q,"kty":%q,"x":%q,"y":%q}`, h.JWK.Crv, h.JWK.Kty, h.JWK.X, h.JWK.Y)
	s := sha256.Sum256([]byte(j))
	return b64(s[:])
}

func (ca *fakeCA) serve(w http.ResponseWriter, r *http.Request) {
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
	var env jws
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &env); err != nil {
		ca.reply(w, 400, map[string]any{"type": "urn:ietf:params:acme:error:malformed", "detail": "bad JWS"}, nil)
		return
	}
	var hdr jwsHeader
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
		ca.acct[base+id] = thumbprint(&hdr)
		ca.mu.Unlock()
		ca.reply(w, 201, map[string]any{"status": "valid"}, map[string]string{"Location": base + id})
	case path == "/order":
		var req struct {
			Identifiers []struct{ Type, Value string } `json:"identifiers"`
		}
		_ = json.Unmarshal(payload, &req)
		if len(req.Identifiers) != 1 {
			ca.reply(w, 400, map[string]any{"type": "urn:ietf:params:acme:error:malformed", "detail": "one identifier"}, nil)
			return
		}
		n := ca.count()
		az := &caAuthz{id: strconv.Itoa(n), domain: req.Identifiers[0].Value, status: "pending",
			httpTok: b64([]byte("http-token-" + strconv.Itoa(n))), alpnTok: b64([]byte("alpn-token-" + strconv.Itoa(n))), acctThmb: thumbOf()}
		o := &caOrder{id: strconv.Itoa(n), domain: az.domain, authz: az.id, status: "pending", acctThmb: az.acctThmb}
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
		var az *caAuthz
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
			ca.validated[parts[1]]++
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
		var req struct {
			CSR string `json:"csr"`
		}
		_ = json.Unmarshal(payload, &req)
		der, _ := base64.RawURLEncoding.DecodeString(req.CSR)
		csr, err := x509.ParseCertificateRequest(der)
		if err != nil || o == nil || csr.CheckSignature() != nil {
			ca.reply(w, 400, map[string]any{"type": "urn:ietf:params:acme:error:badCSR"}, nil)
			return
		}
		pemChain := ca.issue(csr, o.domain)
		ca.mu.Lock()
		o.status, o.certID = "valid", o.id
		ca.certs[o.id] = pemChain
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

func (ca *fakeCA) count() int { ca.mu.Lock(); defer ca.mu.Unlock(); ca.counter++; return ca.counter }

func (ca *fakeCA) orderJSON(o *caOrder) map[string]any {
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

func (ca *fakeCA) authzJSON(az *caAuthz) map[string]any {
	base := ca.srv.URL
	ca.mu.Lock()
	st := az.status
	ca.mu.Unlock()
	return map[string]any{
		"status": st, "identifier": map[string]string{"type": "dns", "value": az.domain},
		"challenges": []map[string]any{
			{"type": "http-01", "url": base + "/chal/" + az.id + "/http-01", "token": az.httpTok, "status": "pending"},
			{"type": "tls-alpn-01", "url": base + "/chal/" + az.id + "/tls-alpn-01", "token": az.alpnTok, "status": "pending"},
		},
	}
}

// validate connects to the node and checks the challenge response, as a real CA does.
func (ca *fakeCA) validate(az *caAuthz, typ string) bool {
	httpAddr, tlsAddr := ca.Resolve(az.domain)
	switch typ {
	case "http-01":
		keyAuth := az.httpTok + "." + az.acctThmb
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + httpAddr + "/.well-known/acme-challenge/" + az.httpTok)
		if err != nil {
			ca.t.Logf("http-01 validation: %v", err)
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return resp.StatusCode == 200 && strings.TrimSpace(string(b)) == keyAuth
	case "tls-alpn-01":
		d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{
			ServerName: az.domain, NextProtos: []string{"acme-tls/1"}, InsecureSkipVerify: true}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := d.DialContext(ctx, "tcp", tlsAddr)
		if err != nil {
			ca.t.Logf("tls-alpn-01 validation: %v", err)
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
		sum := sha256.Sum256([]byte(az.alpnTok + "." + az.acctThmb))
		want, _ := asn1.Marshal(sum[:])
		for _, e := range leaf.Extensions {
			if e.Id.Equal(asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 31}) {
				return string(e.Value) == string(want)
			}
		}
		return false
	}
	return false
}

func (ca *fakeCA) issue(csr *x509.CertificateRequest, domain string) []byte {
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	ca.mu.Lock()
	validity := ca.validity
	ca.issued++
	ca.mu.Unlock()
	tpl := &x509.Certificate{
		SerialNumber: serial, DNSNames: []string{domain},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(validity),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.caCert, csr.PublicKey, ca.caKey)
	if err != nil {
		ca.t.Fatal(err)
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.caDER})...)
}
