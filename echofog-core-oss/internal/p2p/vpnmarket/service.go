package vpnmarket

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	refreshInterval  = 5 * time.Minute
	probeStaleAfter  = 30 * time.Minute
	autoProbesPerRun = 3
	maxProbeDomains  = 16
	domainsKey       = "vpnmarket:domains"
)

// earlyRefresh are the waits before the first directory passes after start
var earlyRefresh = []time.Duration{10 * time.Second, 30 * time.Second, 90 * time.Second}

// DefaultProbeDomains are checked on every exit unless the user sets their own list
var DefaultProbeDomains = []string{"www.cloudflare.com:443", "www.wikipedia.org:443", "www.google.com:443"}

// Tunneler opens and immediately closes a tunnel through an exit, which spends no payload bytes
type Tunneler interface {
	TestDial(ctx context.Context, exit types.PeerID, target string) (time.Duration, error)
}

// Bandwidth measures the rate at which a peer can send to us
type Bandwidth interface {
	MeasureDownload(ctx context.Context, peer types.PeerID) (int, error)
}

// Connector makes sure there is a live connection to a peer
type Connector interface {
	Ensure(ctx context.Context, peer types.PeerID) error
}

// Service ties publishing, discovery, probing and ratings together
type Service struct {
	Publisher *Publisher
	Directory *Directory
	Ratings   *RatingStore

	tunnel    Tunneler
	bandwidth Bandwidth
	connector Connector
	store     storage.Storage
	now       func() time.Time

	mu        sync.Mutex
	autoProbe bool
	lastSync  time.Time
}

func NewService(pub *Publisher, dir *Directory, ratings *RatingStore, st storage.Storage, t Tunneler, b Bandwidth, c Connector) *Service {
	return &Service{Publisher: pub, Directory: dir, Ratings: ratings, tunnel: t, bandwidth: b, connector: c, store: st, now: time.Now, autoProbe: true}
}

// Domains returns the domains checked on every probe
func (s *Service) Domains() []string {
	if raw, err := s.store.Get([]byte(domainsKey)); err == nil && len(raw) > 0 {
		var d []string
		if json.Unmarshal(raw, &d) == nil && len(d) > 0 {
			return d
		}
	}
	return append([]string(nil), DefaultProbeDomains...)
}

// SetDomains stores the list of host:port targets used to check availability through exits
func (s *Service) SetDomains(domains []string) error {
	if len(domains) > maxProbeDomains {
		return errors.New("at most 16 sites can be checked")
	}
	for _, d := range domains {
		if len(d) == 0 || len(d) > 253 {
			return errors.New("invalid domain")
		}
	}
	raw, err := json.Marshal(domains)
	if err != nil {
		return err
	}
	return s.store.Set([]byte(domainsKey), raw)
}

// Probe checks one exit: tunnel reachability to each domain and the real download speed it can deliver
func (s *Service) Probe(ctx context.Context, peer types.PeerID) (ProbeResult, error) {
	res := ProbeResult{At: s.now()}

	if err := s.connector.Ensure(ctx, peer); err != nil {
		_ = s.Ratings.RecordProbe(peer, res)
		return res, err
	}

	var latencySum time.Duration
	for _, target := range s.Domains() {
		lat, err := s.tunnel.TestDial(ctx, peer, target)
		switch {
		case errors.Is(err, exit_gateway.ErrPaymentRequired):
			res.PaymentRequired = true
		case err == nil:
			res.DomainsOK++
			latencySum += lat
		}
		if !res.PaymentRequired {
			res.DomainsTried++
		}
		if res.PaymentRequired {
			break
		}
	}
	if res.DomainsOK > 0 {
		res.LatencyMs = float64(latencySum.Milliseconds()) / float64(res.DomainsOK)
	}
	res.OK = res.DomainsOK > 0 || res.PaymentRequired

	if res.OK && s.bandwidth != nil {
		bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if kbps, err := s.bandwidth.MeasureDownload(bctx, peer); err == nil {
			res.MeasuredKbps = kbps
		}
		cancel()
	}

	if err := s.Ratings.RecordProbe(peer, res); err != nil {
		return res, err
	}
	return res, nil
}

// SetAutoProbe toggles background checks of discovered exits
func (s *Service) SetAutoProbe(on bool) {
	s.mu.Lock()
	s.autoProbe = on
	s.mu.Unlock()
}

// SyncOnce refreshes the directory and probes the exits that were not checked recently
func (s *Service) SyncOnce(ctx context.Context) int {
	n := s.Directory.Refresh(ctx)
	s.mu.Lock()
	s.lastSync = s.now()
	auto := s.autoProbe
	s.mu.Unlock()
	if !auto {
		return n
	}

	probed := 0
	cutoff := s.now().Add(-probeStaleAfter).Unix()
	for _, e := range s.Directory.List() {
		if probed >= autoProbesPerRun || ctx.Err() != nil {
			break
		}
		if e.Rating.LastProbe > cutoff {
			continue
		}
		id, err := e.Advert.ID()
		if err != nil {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		_, _ = s.Probe(pctx, id)
		cancel()
		probed++
	}
	return n
}

// LastSync returns when the directory was last refreshed
func (s *Service) LastSync() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSync
}

// Run refreshes the directory in the background until ctx ends
func (s *Service) Run(ctx context.Context) {
	go func() {
		// the first passes come quickly: right after start the node is still meeting its peers,
		// and a list that waits five minutes for the next pass looks like there are no exits
		for i := 0; ; i++ {
			wait := refreshInterval
			if i < len(earlyRefresh) {
				wait = earlyRefresh[i]
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			s.SyncOnce(ctx)
		}
	}()
}
