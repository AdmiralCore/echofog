package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// An exit resolves the names its clients tunnel to. Asking the machine's resolver sends every name
// in the clear to whoever runs it, usually the hosting provider, and to anyone on the path. The exit
// asks DNS-over-HTTPS servers instead (RFC 8484): the names travel inside TLS, padded to a block
// size (RFC 8467), and the servers are reached by IP address, so finding them needs no lookup in
// the clear either.

// DefaultExitDNS are the DNS-over-HTTPS servers an exit asks when its operator named none, tried in
// this order: Cloudflare and Quad9, both by IP address (their certificates cover the addresses).
var DefaultExitDNS = []string{"https://1.1.1.1/dns-query", "https://9.9.9.9/dns-query"}

// ExitDNSSystem, as the only entry of the server list, keeps the machine's own resolver.
const ExitDNSSystem = "system"

const (
	dohTimeout     = 4 * time.Second // one server's answer; then the next server is asked
	dohMaxAnswer   = 64 << 10
	dohMinTTL      = 30 * time.Second
	dohMaxTTL      = 10 * time.Minute
	dohNegativeTTL = 30 * time.Second // a name that does not exist
	dohCacheSize   = 4096
	dohPadBlock    = 128 // queries are padded to a multiple of this (RFC 8467)
	ednsPadding    = 12  // EDNS(0) option code of the padding
)

var errNoSuchHost = errors.New("no such host")

// dohResolver resolves names over DNS-over-HTTPS with a small cache. When every server fails it
// asks the system resolver, unless fallback is off.
type dohResolver struct {
	servers  []string
	fallback func(ctx context.Context, host string) ([]net.IP, error) // nil: none
	client   *http.Client
	now      func() time.Time

	mu       sync.Mutex
	cache    map[string]dohEntry
	inflight map[string]*dohCall
}

type dohEntry struct {
	ips     []net.IP // nil: the name does not exist
	expires time.Time
}

type dohCall struct {
	done chan struct{}
	ips  []net.IP
	err  error
}

func newDoHResolver(servers []string, fallback func(ctx context.Context, host string) ([]net.IP, error)) *dohResolver {
	tr := &http.Transport{
		Proxy:               nil, // never through a proxy the environment may name
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: dohTimeout,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &dohResolver{
		servers:  append([]string(nil), servers...),
		fallback: fallback,
		client:   &http.Client{Transport: tr},
		now:      time.Now,
		cache:    make(map[string]dohEntry),
		inflight: make(map[string]*dohCall),
	}
}

// LookupIP returns the IPv4 and IPv6 addresses of host.
func (r *dohResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if name == "" {
		return nil, errNoSuchHost
	}
	r.mu.Lock()
	if e, ok := r.cache[name]; ok && r.now().Before(e.expires) {
		r.mu.Unlock()
		if e.ips == nil {
			return nil, errNoSuchHost
		}
		return append([]net.IP(nil), e.ips...), nil
	}
	c, ok := r.inflight[name]
	if !ok {
		c = &dohCall{done: make(chan struct{})}
		r.inflight[name] = c
		// the lookup outlives the caller that started it: others may be waiting on it
		go func() {
			lctx, cancel := context.WithTimeout(context.Background(), dohTimeout*time.Duration(len(r.servers)+1))
			c.ips, c.err = r.resolve(lctx, name)
			cancel()
			r.mu.Lock()
			delete(r.inflight, name)
			r.mu.Unlock()
			close(c.done)
		}()
	}
	r.mu.Unlock()
	select {
	case <-c.done:
		return append([]net.IP(nil), c.ips...), c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// resolve asks the servers in turn and caches the first real answer.
func (r *dohResolver) resolve(ctx context.Context, name string) ([]net.IP, error) {
	var lastErr error
	for _, srv := range r.servers {
		ips, ttl, err := r.ask(ctx, srv, name)
		switch {
		case errors.Is(err, errNoSuchHost):
			r.store(name, nil, dohNegativeTTL)
			return nil, err
		case err == nil:
			r.store(name, ips, ttl)
			return ips, nil
		}
		lastErr = err
	}
	if r.fallback != nil {
		return r.fallback(ctx, name)
	}
	if lastErr == nil {
		lastErr = errors.New("no DNS server configured")
	}
	return nil, lastErr
}

func (r *dohResolver) store(name string, ips []net.IP, ttl time.Duration) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= dohCacheSize {
		for k, e := range r.cache {
			if !now.Before(e.expires) {
				delete(r.cache, k)
			}
		}
		for k := range r.cache { // still full: drop entries at random
			if len(r.cache) < dohCacheSize {
				break
			}
			delete(r.cache, k)
		}
	}
	r.cache[name] = dohEntry{ips: ips, expires: now.Add(ttl)}
}

// ask resolves name at one server: A and AAAA at once. It returns errNoSuchHost for an
// authoritative "does not exist", and the shortest TTL of the answers, clamped.
func (r *dohResolver) ask(ctx context.Context, srv, name string) ([]net.IP, time.Duration, error) {
	type result struct {
		ips  []net.IP
		ttl  uint32
		none bool // NXDOMAIN
		err  error
	}
	types := []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA}
	results := make(chan result, len(types))
	qctx, cancel := context.WithTimeout(ctx, dohTimeout)
	defer cancel()
	for _, t := range types {
		go func(t dnsmessage.Type) {
			ips, ttl, none, err := r.query(qctx, srv, name, t)
			results <- result{ips, ttl, none, err}
		}(t)
	}
	var (
		ips     []net.IP
		minTTL  = uint32(dohMaxTTL / time.Second)
		none    int
		lastErr error
	)
	for range types {
		res := <-results
		switch {
		case res.err != nil:
			lastErr = res.err
		case res.none:
			none++
		default:
			ips = append(ips, res.ips...)
			if len(res.ips) > 0 {
				minTTL = min(minTTL, res.ttl)
			}
		}
	}
	ttl := max(dohMinTTL, min(dohMaxTTL, time.Duration(minTTL)*time.Second))
	switch {
	case len(ips) > 0:
		return ips, ttl, nil
	case lastErr != nil:
		return nil, 0, lastErr
	case none > 0:
		return nil, 0, errNoSuchHost
	}
	return nil, 0, fmt.Errorf("%s: no addresses for the name", srv) // NODATA for both: let the next server try
}

// query sends one DNS-over-HTTPS POST for name/t.
func (r *dohResolver) query(ctx context.Context, srv, name string, t dnsmessage.Type) ([]net.IP, uint32, bool, error) {
	q, err := buildDoHQuery(name, t)
	if err != nil {
		return nil, 0, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv, bytes.NewReader(q))
	if err != nil {
		return nil, 0, false, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, false, fmt.Errorf("%s: HTTP %d", srv, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, dohMaxAnswer))
	if err != nil {
		return nil, 0, false, err
	}
	var p dnsmessage.Parser
	h, err := p.Start(body)
	if err != nil {
		return nil, 0, false, fmt.Errorf("%s: bad answer: %w", srv, err)
	}
	switch h.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError:
		return nil, 0, true, nil
	default:
		return nil, 0, false, fmt.Errorf("%s: %v", srv, h.RCode)
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, 0, false, fmt.Errorf("%s: bad answer: %w", srv, err)
	}
	var (
		ips []net.IP
		ttl = ^uint32(0)
	)
	for {
		ah, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil, 0, false, fmt.Errorf("%s: bad answer: %w", srv, err)
		}
		switch {
		case ah.Type == dnsmessage.TypeA && t == dnsmessage.TypeA:
			a, err := p.AResource()
			if err != nil {
				return nil, 0, false, err
			}
			ips = append(ips, net.IP(append([]byte(nil), a.A[:]...)))
			ttl = min(ttl, ah.TTL)
		case ah.Type == dnsmessage.TypeAAAA && t == dnsmessage.TypeAAAA:
			a, err := p.AAAAResource()
			if err != nil {
				return nil, 0, false, err
			}
			ips = append(ips, net.IP(append([]byte(nil), a.AAAA[:]...)))
			ttl = min(ttl, ah.TTL)
		default:
			if err := p.SkipAnswer(); err != nil { // the CNAME chain and anything else
				return nil, 0, false, err
			}
		}
	}
	return ips, ttl, false, nil
}

// buildDoHQuery packs a recursive query for name/t with ID 0 (RFC 8484 4.1) and EDNS(0) padding
// up to a multiple of dohPadBlock, so the length does not tell the name.
func buildDoHQuery(name string, t dnsmessage.Type) ([]byte, error) {
	n, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return nil, err
	}
	pack := func(pad int) ([]byte, error) {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{RecursionDesired: true})
		b.EnableCompression()
		if err := b.StartQuestions(); err != nil {
			return nil, err
		}
		if err := b.Question(dnsmessage.Question{Name: n, Type: t, Class: dnsmessage.ClassINET}); err != nil {
			return nil, err
		}
		if err := b.StartAdditionals(); err != nil {
			return nil, err
		}
		var rh dnsmessage.ResourceHeader
		if err := rh.SetEDNS0(4096, dnsmessage.RCodeSuccess, false); err != nil {
			return nil, err
		}
		if err := b.OPTResource(rh, dnsmessage.OPTResource{Options: []dnsmessage.Option{{Code: ednsPadding, Data: make([]byte, pad)}}}); err != nil {
			return nil, err
		}
		return b.Finish()
	}
	msg, err := pack(0)
	if err != nil {
		return nil, err
	}
	if rem := len(msg) % dohPadBlock; rem != 0 {
		return pack(dohPadBlock - rem)
	}
	return msg, nil
}

// SetDNS sets how the exit resolves the names clients tunnel to: servers are DNS-over-HTTPS URLs
// tried in order (nil or empty: DefaultExitDNS), or just ExitDNSSystem for the machine's resolver.
// With fallback the machine's resolver is asked when no server answers, so the exit keeps
// working where those servers are blocked, at the price of those names going out in the clear.
// Call it before the exit serves anyone.
func (e *ExitService) SetDNS(servers []string, fallback bool) error {
	system := func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", host)
	}
	if len(servers) == 1 && servers[0] == ExitDNSSystem {
		e.resolver = system
		return nil
	}
	if len(servers) == 0 {
		servers = DefaultExitDNS
	}
	for _, s := range servers {
		if !strings.HasPrefix(s, "https://") {
			return fmt.Errorf("exit DNS server %q: give an https:// DNS-over-HTTPS URL, or %q alone", s, ExitDNSSystem)
		}
	}
	var fb func(ctx context.Context, host string) ([]net.IP, error)
	if fallback {
		fb = system
	}
	e.resolver = newDoHResolver(servers, fb).LookupIP
	return nil
}
