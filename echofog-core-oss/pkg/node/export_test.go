package node

import (
	"crypto/x509"

	"github.com/echofog/echofog-core/internal/protection/osprotect"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// SetSiteCheckRoots lets tests trust their own certificate authority.
func SetSiteCheckRoots(p *x509.CertPool) { siteCheckRoots = p }

// SetRememberProtector stands in for the OS protector of the remembered unlock; the returned
// function puts the real one back.
func SetRememberProtector(p osprotect.Protector) (restore func()) {
	old := rememberProtector
	rememberProtector = p
	return func() { rememberProtector = old }
}

// IdentityPeerID is the logged-in identity's peer ID, before the node starts.
func (n *Node) IdentityPeerID() (types.PeerID, error) { return n.identity.GetPeerID() }

// ResealIdentity seals the identity root again with another password, as a password change would.
func (n *Node) ResealIdentity(appPassword []byte) error { return n.identity.Save(appPassword) }

// RememberFile is where cfg's remembered unlock is kept.
var RememberFile = rememberPath
