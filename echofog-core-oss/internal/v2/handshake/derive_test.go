package handshake

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

func TestTLSBindingBothEnds(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"x"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	a, b := net.Pipe()
	srv := tls.Server(a, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	cli := tls.Client(b, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	errc := make(chan error, 1)
	go func() { errc <- srv.Handshake() }()
	if err := cli.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	cs1, cs2 := cli.ConnectionState(), srv.ConnectionState()
	c1, err := TLSBinding(&cs1)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := TLSBinding(&cs2)
	direct, _ := cs1.ExportKeyingMaterial("EXPORTER-echofog/hs2/channel", nil, 32)
	if len(c1) != 32 || !bytes.Equal(c1, c2) || !bytes.Equal(c1, direct) {
		t.Fatal("exporter values differ")
	}
	old := tls.ConnectionState{Version: tls.VersionTLS12}
	if _, err := TLSBinding(&old); err == nil {
		t.Fatal("TLS 1.2 accepted")
	}
	cli.Close()
	srv.Close()
}
