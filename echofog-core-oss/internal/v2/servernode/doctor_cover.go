package servernode

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Limits of the cover preflight.
const (
	coverCertWarnDays = 14
	coverCheckTimeout = 10 * time.Second
	coverSiteMaxBytes = 64 << 10
	coverMountInfo    = "/proc/self/mountinfo"
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

// RunCoverDoctor checks what a tester sees before the owner publishes the node: the name points at
// the advertised address, the port answers, the handshake is TLS 1.3 with h2 behind a trusted
// certificate, and the site is the operator's own HTML, present and mounted.
func RunCoverDoctor(ctx context.Context, d CoverDeps) []CheckResult {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Dial == nil {
		dialer := &net.Dialer{Timeout: coverCheckTimeout}
		d.Dial = dialer.DialContext
	}
	var out []CheckResult
	add := func(id string, l Level, code string, args ...any) {
		out = append(out, CheckResult{ID: id, Level: l, Code: code, Args: args})
	}
	if strings.TrimSpace(d.Cover) == "" {
		add("cover", LevelFail, "cover.nocover")
		return out
	}
	host, port, err := net.SplitHostPort(d.Cover)
	if err != nil || host == "" || port == "" {
		add("cover.dns", LevelFail, "cover.cfg.bad", d.Cover)
		return out
	}
	out = append(out,
		coverDNSCheck(ctx, d, host),
		coverPortCheck(ctx, d),
		coverTLSCheck(ctx, d, host),
		coverSiteCheck(ctx, d, host, port),
		coverFilesCheck(d),
		coverMountCheck(d),
	)
	return out
}

// coverDNSCheck compares the name with the advertised address. The resolver returns the addresses
// of the whole CNAME chain, so a name that is an alias is checked at its end.
func coverDNSCheck(ctx context.Context, d CoverDeps, host string) CheckResult {
	res := func(l Level, code string, args ...any) CheckResult {
		return CheckResult{ID: "cover.dns", Level: l, Code: code, Args: args}
	}
	want, err := netip.ParseAddr(strings.TrimSpace(d.Advertise))
	if err != nil {
		return res(LevelFail, "cover.dns.noadvertise", host)
	}
	want = want.Unmap()
	if d.Resolve == nil {
		return res(LevelInfo, "cover.dns.skipped", host)
	}
	addrs, err := d.Resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return res(LevelFail, "cover.dns.none", host, want.String())
	}
	found := false
	for _, a := range addrs {
		found = found || a.Unmap() == want
	}
	switch {
	case found && len(addrs) == 1:
		return res(LevelOK, "cover.dns.ok", host)
	case found:
		return res(LevelWarn, "cover.dns.partial", host)
	}
	return res(LevelFail, "cover.dns.mismatch", host, want.String())
}

func coverPortCheck(ctx context.Context, d CoverDeps) CheckResult {
	res := func(l Level, code string, args ...any) CheckResult {
		return CheckResult{ID: "cover.port", Level: l, Code: code, Args: args}
	}
	conn, err := d.Dial(ctx, "tcp", d.Cover)
	if err != nil {
		return res(LevelFail, "cover.port.closed", d.Cover, shortErr(err))
	}
	_ = conn.Close()
	return res(LevelOK, "cover.port.ok", d.Cover)
}

// coverTLSCheck is the strict handshake: TLS 1.3, h2 in ALPN and a certificate that a browser
// accepts. The handshake runs without the built-in check so that the operator gets the exact
// reason; the chain, the name and the term are verified below, and a certificate that fails them
// never passes.
func coverTLSCheck(ctx context.Context, d CoverDeps, host string) CheckResult {
	res := func(l Level, code string, args ...any) CheckResult {
		return CheckResult{ID: "cover.tls", Level: l, Code: code, Args: args}
	}
	raw, err := d.Dial(ctx, "tcp", d.Cover)
	if err != nil {
		return res(LevelFail, "cover.tls.handshake", shortErr(err))
	}
	defer raw.Close()
	_ = raw.SetDeadline(d.Now().Add(coverCheckTimeout))
	conn := tls.Client(raw, &tls.Config{
		ServerName:         host,
		NextProtos:         []string{"h2", "http/1.1"},
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // verified by hand below, with the reason reported
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		return res(LevelFail, "cover.tls.handshake", shortErr(err))
	}
	st := conn.ConnectionState()
	if st.Version != tls.VersionTLS13 {
		return res(LevelFail, "cover.tls.version", tls.VersionName(st.Version))
	}
	if st.NegotiatedProtocol != "h2" {
		proto := st.NegotiatedProtocol
		if proto == "" {
			proto = "none"
		}
		return res(LevelFail, "cover.tls.alpn", proto)
	}
	if len(st.PeerCertificates) == 0 {
		return res(LevelFail, "cover.tls.cert", "the server sent no certificate")
	}
	leaf := st.PeerCertificates[0]
	if isSelfSigned(leaf) {
		return res(LevelFail, "cover.tls.selfsigned", host)
	}
	intermediates := x509.NewCertPool()
	for _, c := range st.PeerCertificates[1:] {
		intermediates.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: d.Roots, Intermediates: intermediates, DNSName: host, CurrentTime: d.Now(),
	}); err != nil {
		return res(LevelFail, "cover.tls.cert", shortErr(err))
	}
	days := int(leaf.NotAfter.Sub(d.Now()).Hours() / 24)
	if days <= coverCertWarnDays {
		return res(LevelWarn, "cover.tls.soon", days)
	}
	return res(LevelOK, "cover.tls.ok", days)
}

// isSelfSigned reports a certificate that names itself as its issuer and signs itself: a chain
// check that has it in the roots would pass, while a browser shows a warning.
func isSelfSigned(c *x509.Certificate) bool {
	if !bytes.Equal(c.RawIssuer, c.RawSubject) {
		return false
	}
	return c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) == nil
}

// coverSiteCheck is the one GET: the cover's own page over the cover's own name. A redirect is not
// followed, so the check never leaves the cover for a host it names.
func coverSiteCheck(ctx context.Context, d CoverDeps, host, port string) CheckResult {
	res := func(l Level, code string, args ...any) CheckResult {
		return CheckResult{ID: "cover.site", Level: l, Code: code, Args: args}
	}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.Dial(ctx, network, d.Cover)
		},
		TLSClientConfig: &tls.Config{
			ServerName: host,
			RootCAs:    d.Roots,
			NextProtos: []string{"h2", "http/1.1"},
			MinVersion: tls.VersionTLS12,
		},
		ForceAttemptHTTP2:   true,
		DisableKeepAlives:   true,
		DisableCompression:  true,
		TLSHandshakeTimeout: coverCheckTimeout,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		Timeout:   coverCheckTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	u := "https://" + net.JoinHostPort(host, port) + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return res(LevelFail, "cover.site.get", shortErr(err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return res(LevelFail, "cover.site.get", shortErr(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, coverSiteMaxBytes))
	if resp.StatusCode != http.StatusOK {
		return res(LevelFail, "cover.site.status", resp.StatusCode)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "text/html") &&
		!strings.Contains(strings.ToLower(http.DetectContentType(body)), "text/html") {
		return res(LevelFail, "cover.site.nothtml", contentType)
	}
	return res(LevelOK, "cover.site.ok")
}

func coverFilesCheck(d CoverDeps) CheckResult {
	res := func(l Level, code string, args ...any) CheckResult {
		return CheckResult{ID: "cover.files", Level: l, Code: code, Args: args}
	}
	if d.SiteDir == "" {
		return res(LevelFail, "cover.files.nodir")
	}
	index := filepath.Join(d.SiteDir, "index.html")
	if _, err := os.Stat(index); err != nil {
		return res(LevelFail, "cover.files.missing", index)
	}
	return res(LevelOK, "cover.files.ok", index)
}

// coverMountCheck reads the mount table: inside a container the site folder must be mounted, and
// where the table cannot be read the check says so instead of guessing.
func coverMountCheck(d CoverDeps) CheckResult {
	res := func(l Level, code string, args ...any) CheckResult {
		return CheckResult{ID: "cover.mount", Level: l, Code: code, Args: args}
	}
	if d.SiteDir == "" {
		return res(LevelInfo, "cover.mount.nodir")
	}
	path := d.MountInfo
	if path == "" {
		path = coverMountInfo
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return res(LevelWarn, "cover.mount.unknown", d.SiteDir)
	}
	if mountedAt(raw, d.SiteDir) {
		return res(LevelOK, "cover.mount.ok", d.SiteDir)
	}
	return res(LevelWarn, "cover.mount.missing", d.SiteDir)
}

// mountedAt reports whether dir is a mount point or lies below one; the root is not counted, it
// is a mount in every container.
func mountedAt(mountinfo []byte, dir string) bool {
	clean := filepath.Clean(dir)
	for _, line := range strings.Split(string(mountinfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		point := fields[4]
		if point == clean {
			return true
		}
		if point != "/" && strings.HasPrefix(clean, strings.TrimSuffix(point, "/")+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
