package clock

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	xproxy "golang.org/x/net/proxy"
)

const (
	// minCDNAnswers is the number of answers D36 requires before an offset can be
	// adopted; it is also the smallest usable endpoint list.
	minCDNAnswers = 3
	// maxEndpoints bounds the number of parallel requests a configuration can cause.
	maxEndpoints = 32
	// checkInterval is D36's "at most once per 10 minutes".
	checkInterval = 10 * time.Minute
	// cdnAgreement is D36's "within 2 s".
	cdnAgreement = 2 * time.Second
	// requestTimeout bounds one HEAD request. It is a context deadline, not a timer
	// the clock sleeps on.
	requestTimeout = 5 * time.Second
	// maxDrain bounds how much of a response body is read before it is closed. Only the
	// Date header is used.
	maxDrain = 512
)

var (
	// ErrTooFewEndpoints means fewer than 3 clock endpoints were configured.
	ErrTooFewEndpoints = errors.New("clock: fewer than 3 CDN endpoints")
	// ErrTooManyEndpoints means more than 32 clock endpoints were configured.
	ErrTooManyEndpoints = errors.New("clock: more than 32 CDN endpoints")
	// ErrInvalidEndpoint means an endpoint is not an absolute https URL.
	ErrInvalidEndpoint = errors.New("clock: endpoint is not an absolute https URL")
	// ErrNoHTTPClient means the CDN check is due but Config.HTTP is nil.
	ErrNoHTTPClient = errors.New("clock: no HTTP client for the CDN check")
)

// defaultEndpoints is the registries.md §12 list.
var defaultEndpoints = []string{
	"https://www.cloudflare.com/",
	"https://www.akamai.com/",
	"https://www.fastly.com/",
	"https://aws.amazon.com/",
	"https://www.microsoft.com/",
	"https://www.apple.com/",
	"https://www.google.com/",
}

// DefaultEndpoints returns a copy of the registries.md §12 clock endpoint list. The list
// is configuration, not protocol: users and distributions may replace it.
func DefaultEndpoints() []string { return slices.Clone(defaultEndpoints) }

// HTTPClient is the single HTTP surface the CDN check needs. In production it is
// node.Host.HTTP (02-architecture §3.1): one client with browser-shaped TLS that, in
// anonymous mode, routes requests through the node's proxy. The clock never builds or
// inspects a transport, so *http.Client satisfies the interface.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// StartupConfig returns the clock configuration a product starts with: the CDN check is
// on and uses the browser-shaped HTTP client (handshake.md D36). A non-nil proxy is the
// node's SOCKS5 proxy (T11.6a); nil means a direct connection.
func StartupConfig(proxy *url.URL, timeout time.Duration) Config {
	return Config{HTTP: NewHTTPClient(proxy, timeout)}
}

// NewHTTPClient returns the client the node uses for its own lookups, the CDN clock check
// first of all: uTLS with the Chrome ClientHello over HTTP/1.1 and the certificate checked
// (handshake.md D36, audit M1 B-4). A non-nil SOCKS5 proxy carries every connection with
// names unresolved; nil means a direct connection. Proxy environment variables are never
// read, and an unusable proxy fails closed instead of falling back to a direct socket.
//
// Tests replace the trust store through the transport's TLSClientConfig.RootCAs, the same
// field the TLS dialer reads.
func NewHTTPClient(proxy *url.URL, timeout time.Duration) *http.Client {
	dial := (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	var dialErr error
	if proxy != nil {
		var d xproxy.Dialer
		if proxy.Host == "" {
			dialErr = ErrInvalidEndpoint
		} else if d, dialErr = xproxy.SOCKS5("tcp", proxy.Host, nil, &net.Dialer{Timeout: 10 * time.Second}); dialErr == nil {
			cd, ok := d.(xproxy.ContextDialer)
			if !ok {
				dialErr = errors.New("clock: the proxy dialer has no context")
			} else {
				dial = cd.DialContext
			}
		}
	}
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dialErr != nil {
			return nil, dialErr
		}
		return dial(ctx, network, addr)
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         dialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 30 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
	}
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dialErr != nil {
			return nil, dialErr
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
		if err != nil {
			return nil, err
		}
		for _, ext := range spec.Extensions {
			if alpn, ok := ext.(*utls.ALPNExtension); ok {
				alpn.AlpnProtocols = []string{"http/1.1"}
			}
		}
		raw, err := dialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		defer stop()
		uc := utls.UClient(raw, &utls.Config{ServerName: host, RootCAs: tr.TLSClientConfig.RootCAs}, utls.HelloCustom)
		if err := uc.ApplyPreset(&spec); err != nil {
			_ = raw.Close()
			return nil, err
		}
		if err := uc.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		return uc, nil
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// Result describes one CDN check. Offset is the clock's current offset after the call,
// including calls that were not due. Warning reports that this call raised a new
// user-facing warning.
type Result struct {
	Ran      bool
	Answers  int
	Agreeing int
	Offset   time.Duration
	Changed  bool
	Warning  bool
}

// MaybeCheckCDN runs the D36 clock check when one is due. allL0Failed reports that the
// caller attempted L0 dials and every one of them failed; an authenticated peer seen in
// the last hour suppresses the check unless that flag is set.
//
// A check is attempted at most once per 10 minutes even when it yields no majority: a
// failed check is still a check, and hammering the endpoints would link requests. The
// offset is adopted only from at least three answers with a strict majority within 2 s.
// Without a majority the current offset stays in force.
func (c *Clock) MaybeCheckCDN(ctx context.Context, allL0Failed bool) (Result, error) {
	raw := c.now()

	c.mu.Lock()
	res := Result{Offset: c.offset}
	sincePeer := raw.Sub(c.lastPeer)
	recentPeer := !c.lastPeer.IsZero() && sincePeer >= 0 && sincePeer < noPeerAfter
	sinceCheck := raw.Sub(c.lastCheck)
	gated := !c.lastCheck.IsZero() && sinceCheck >= 0 && sinceCheck < checkInterval
	if (!allL0Failed && recentPeer) || c.checking || gated {
		c.mu.Unlock()
		return res, nil
	}
	client := c.http
	endpoints := slices.Clone(c.endpoints)
	if client == nil {
		c.mu.Unlock()
		return res, ErrNoHTTPClient
	}
	c.checking = true
	c.lastCheck = raw
	c.mu.Unlock()

	res.Ran = true
	answers := fetchDeltas(ctx, client, endpoints, c.now)
	res.Answers = len(answers)
	offset, agreeing, ok := majorityOffset(answers)
	res.Agreeing = agreeing

	c.mu.Lock()
	c.checking = false
	if ok {
		res.Changed = offset != c.offset
		res.Warning = c.setOffsetLocked(offset)
	}
	res.Offset = c.offset
	c.mu.Unlock()

	if res.Warning && c.warn != nil {
		c.warn(res.Offset)
	}
	return res, nil
}

// fetchDeltas requests every endpoint in parallel and returns the usable offsets. It
// reads Date only; a transport error, a missing header and an unparsable header all mean
// "no answer" and are not returned as errors.
func fetchDeltas(ctx context.Context, client HTTPClient, endpoints []string, now func() time.Time) []time.Duration {
	type answer struct {
		offset time.Duration
		ok     bool
	}
	results := make([]answer, len(endpoints))

	var wg sync.WaitGroup
	for i, endpoint := range endpoints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			offset, ok := fetchDate(ctx, client, endpoint, now)
			results[i] = answer{offset: offset, ok: ok}
		}()
	}
	wg.Wait()

	offsets := make([]time.Duration, 0, len(results))
	for _, r := range results {
		if r.ok {
			offsets = append(offsets, r.offset)
		}
	}
	return offsets
}

func fetchDate(ctx context.Context, client HTTPClient, endpoint string, now func() time.Time) (time.Duration, bool) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, endpoint, nil)
	if err != nil {
		return 0, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	if resp.Body != nil {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	}

	header := resp.Header.Get("Date")
	if header == "" {
		return 0, false
	}
	serverTime, err := http.ParseTime(header)
	if err != nil {
		return 0, false
	}
	// Date has one-second resolution; compare whole seconds so that honest endpoints
	// with slightly different request times differ by at most a second.
	return serverTime.Truncate(time.Second).Sub(now().Truncate(time.Second)), true
}

// majorityOffset returns the median of the largest cluster of offsets that agree within
// D36's 2 s, the size of that cluster, and whether it is a strict majority. Ties between
// clusters are resolved by tighter span and then by the smaller absolute offset, so the
// result does not depend on request completion order.
func majorityOffset(offsets []time.Duration) (time.Duration, int, bool) {
	if len(offsets) < minCDNAnswers {
		return 0, 0, false
	}
	sorted := slices.Clone(offsets)
	slices.Sort(sorted)

	bestCount, bestSpan := 0, time.Duration(0)
	best := time.Duration(0)
	for i := range sorted {
		j := i
		for j+1 < len(sorted) && sorted[j+1]-sorted[i] <= cdnAgreement {
			j++
		}
		count := j - i + 1
		if count*2 <= len(sorted) {
			continue
		}
		span := sorted[j] - sorted[i]
		median := sorted[i+(count-1)/2]
		better := count > bestCount ||
			(count == bestCount && (span < bestSpan ||
				(span == bestSpan && absDuration(median) < absDuration(best))))
		if better {
			bestCount, bestSpan, best = count, span, median
		}
	}
	if bestCount == 0 {
		return 0, 0, false
	}
	return best, bestCount, true
}

func validEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Host != "" && u.User == nil
}
