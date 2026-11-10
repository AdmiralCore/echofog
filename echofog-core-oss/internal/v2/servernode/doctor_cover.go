package servernode

import (
	"context"
	"crypto/x509"
	"net"
	"net/netip"
	"time"
)

// CoverDeps are the inputs of the cover preflight (`doctor --cover`). Besides reading local files
// it sends DNS queries, one TCP probe, one TLS handshake and one GET, all to the cover's own name
// and address: nothing else leaves the machine.
type CoverDeps struct {
	// Cover is the host:port the cover answers on.
	Cover string
	// Advertise is the IP literal the cover name must resolve to.
	Advertise string
	// SiteDir is the folder with the site inside the container.
	SiteDir string
	Now     func() time.Time
	// Resolve looks the cover name up; nil skips the DNS check.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial opens the connections of the port, TLS and site checks; nil uses the network stack.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Roots is the trust store of the TLS check; nil means the system roots.
	Roots *x509.CertPool
	// MountInfo is the path of the mount table; empty means /proc/self/mountinfo.
	MountInfo string
}

// RunCoverDoctor runs the cover preflight checks.
func RunCoverDoctor(ctx context.Context, d CoverDeps) []CheckResult {
	return nil
}
