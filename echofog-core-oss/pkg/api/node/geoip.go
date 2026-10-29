package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
)

// Where an exit is, looked up online from the IP it was reached at. While connected the lookup
// goes through the exit itself, so the GeoIP service sees the exit asking about its own address,
// not this computer listing the exits it uses. Results are kept in the sealed database.
const (
	geoCacheKey   = "vpnmarket:geo"
	geoKeep       = 30 * 24 * time.Hour // a hoster's address does not move country often
	geoRetryAfter = 6 * time.Hour       // a failed lookup is not repeated sooner
	geoTimeout    = 8 * time.Second
	maxGeoEntries = 500
)

// geoServices are tried in order; both answer over HTTPS without an API key.
var geoServices = []geoService{
	{url: "https://ipwho.is/%s", parse: parseIPWhoIs},
	{url: "https://ipinfo.io/%s/json", parse: parseIPInfo},
}

type geoService struct {
	url   string // %s is the IP
	parse func([]byte) (geoEntry, error)
}

// geoEntry is one cached answer; At is when it was looked up, Failed marks a lookup that found
// nothing (kept so it is not retried at once).
type geoEntry struct {
	CountryCode string `json:"cc,omitempty"`
	City        string `json:"city,omitempty"`
	Org         string `json:"org,omitempty"` // the provider (ISP) of the address, when the service names it
	At          int64  `json:"at"`
	Failed      bool   `json:"failed,omitempty"`
}

func parseIPWhoIs(b []byte) (geoEntry, error) {
	var r struct {
		Success     bool   `json:"success"`
		CountryCode string `json:"country_code"`
		City        string `json:"city"`
		Connection  struct {
			ISP string `json:"isp"`
			Org string `json:"org"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return geoEntry{}, err
	}
	if !r.Success || len(r.CountryCode) != 2 {
		return geoEntry{}, errors.New("no location")
	}
	org := r.Connection.ISP
	if org == "" {
		org = r.Connection.Org
	}
	return geoEntry{CountryCode: strings.ToUpper(r.CountryCode), City: r.City, Org: org}, nil
}

func parseIPInfo(b []byte) (geoEntry, error) {
	var r struct {
		Country string `json:"country"`
		City    string `json:"city"`
		Org     string `json:"org"` // "AS12389 PJSC Rostelecom"
		Bogon   bool   `json:"bogon"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return geoEntry{}, err
	}
	if r.Bogon || len(r.Country) != 2 {
		return geoEntry{}, errors.New("no location")
	}
	org := r.Org
	if as, name, ok := strings.Cut(org, " "); ok && strings.HasPrefix(as, "AS") {
		org = name
	}
	return geoEntry{CountryCode: strings.ToUpper(r.Country), City: r.City, Org: org}, nil
}

// geoLocator looks addresses up once each and keeps the answers.
type geoLocator struct {
	mu       sync.Mutex
	inFlight map[string]bool
	services []geoService
	client   func() *http.Client // through the exit when connected, else direct
}

func loadGeoCache(st storage.Storage) map[string]geoEntry {
	out := map[string]geoEntry{}
	if st == nil {
		return out
	}
	if raw, err := st.Get([]byte(geoCacheKey)); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func saveGeoCache(st storage.Storage, m map[string]geoEntry) {
	if st == nil {
		return
	}
	if len(m) > maxGeoEntries {
		var oldest string
		for ip, e := range m {
			if oldest == "" || e.At < m[oldest].At {
				oldest = ip
			}
		}
		delete(m, oldest)
	}
	if raw, err := json.Marshal(m); err == nil {
		_ = st.Set([]byte(geoCacheKey), raw)
	}
}

// geoIP is the public IP of addr ("host:port"), or "" for a name that does not resolve or an
// address no service can place (private, loopback).
func geoIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", host)
		if err != nil || len(ips) == 0 {
			return ""
		}
		ip = ips[0]
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || isCGNATAddr(ip) {
		return ""
	}
	return ip.String()
}

func isCGNATAddr(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xC0 == 64
}

// cached returns what is known about ip, and whether a lookup is due.
func cachedGeo(m map[string]geoEntry, ip string, now time.Time) (geoEntry, bool) {
	e, ok := m[ip]
	if !ok {
		return geoEntry{}, true
	}
	age := now.Sub(time.Unix(e.At, 0))
	if e.Failed {
		return geoEntry{}, age > geoRetryAfter
	}
	return e, age > geoKeep
}

// lookup asks the services in turn and returns the first answer.
func (g *geoLocator) lookup(ctx context.Context, ip string) (geoEntry, error) {
	var last error = errors.New("no GeoIP service")
	for _, s := range g.services {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.Replace(s.url, "%s", url.PathEscape(ip), 1), nil)
		if err != nil {
			return geoEntry{}, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := g.client().Do(req)
		if err != nil {
			last = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			last = errors.New("GeoIP service answered " + resp.Status)
			continue
		}
		e, err := s.parse(body)
		if err != nil {
			last = err
			continue
		}
		return e, nil
	}
	return geoEntry{}, last
}

// refresh looks up, in the background, the addresses that have no fresh answer yet.
func (g *geoLocator) refresh(st storage.Storage, addrs []string, done func()) {
	now := time.Now()
	cache := loadGeoCache(st)
	var due []string
	g.mu.Lock()
	for _, a := range addrs {
		ip := geoIP(a)
		if ip == "" || g.inFlight[ip] {
			continue
		}
		if _, stale := cachedGeo(cache, ip, now); stale {
			g.inFlight[ip] = true
			due = append(due, ip)
		}
	}
	g.mu.Unlock()
	if len(due) == 0 {
		return
	}
	go func() {
		changed := false
		for _, ip := range due {
			ctx, cancel := context.WithTimeout(context.Background(), geoTimeout)
			e, err := g.lookup(ctx, ip)
			cancel()
			if err != nil {
				e = geoEntry{Failed: true}
			}
			e.At = time.Now().Unix()
			g.mu.Lock()
			c := loadGeoCache(st)
			c[ip] = e
			saveGeoCache(st, c)
			delete(g.inFlight, ip)
			g.mu.Unlock()
			changed = changed || !e.Failed
		}
		if changed && done != nil {
			done()
		}
	}()
}

// geoClient is the HTTP client for lookups: through the running proxy, so the request leaves
// from the exit, or the normal one when not connected.
func (n *NodeApi) geoClient() *http.Client {
	n.vpn.mu.Lock()
	srv := n.vpn.server
	n.vpn.mu.Unlock()
	if srv != nil {
		if a := srv.Addr(); a != nil {
			proxy := &url.URL{Scheme: "socks5", Host: a.String()}
			return &http.Client{Timeout: geoTimeout, Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
		}
	}
	return &http.Client{Timeout: geoTimeout}
}

func (n *NodeApi) geo() *geoLocator {
	n.geoOnce.Do(func() {
		n.geoLoc = &geoLocator{inFlight: map[string]bool{}, services: geoServices, client: n.geoClient}
	})
	return n.geoLoc
}

// locateExits fills in where each exit is from the cache, and looks up the rest in the
// background; the UI hears "vpn:exits_located" when new answers arrive.
func (n *NodeApi) locateExits(exits []SavedExit) {
	st := n.historyStorage()
	cache := loadGeoCache(st)
	now := time.Now()
	var addrs []string
	for i := range exits {
		if exits[i].Addr == "" {
			continue
		}
		addrs = append(addrs, exits[i].Addr)
		if ip := geoIP(exits[i].Addr); ip != "" {
			if e, _ := cachedGeo(cache, ip, now); e.CountryCode != "" {
				exits[i].CountryCode, exits[i].City = e.CountryCode, e.City
			}
		}
	}
	n.geo().refresh(st, addrs, func() { n.emit("vpn:exits_located", nil) })
}
