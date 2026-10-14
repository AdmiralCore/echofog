package node

import "crypto/x509"

// SetSiteCheckRoots lets tests trust their own certificate authority.
func SetSiteCheckRoots(p *x509.CertPool) { siteCheckRoots = p }
