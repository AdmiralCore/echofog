package node

import (
	"crypto/x509"
	"errors"
)

// Egress (T11.6a): the node's own traffic through a SOCKS5 proxy. Stub until the fix.
type Egress struct {
	// Proxy is the SOCKS5 proxy, host:port.
	Proxy string
	// Tor says the proxy is Tor (anonymous = tor).
	Tor bool
	// RootCAs, when set, replaces the system roots for Host.HTTP() (tests).
	RootCAs *x509.CertPool
}

// EgressStatus is what runs.
type EgressStatus struct {
	Active bool
	Tor    bool
	Proxy  string
}

// EgressStatus reports the proxy mode in effect.
func (r *Runtime) EgressStatus() EgressStatus { return EgressStatus{} }

var errEgressStub = errors.New("stub")
