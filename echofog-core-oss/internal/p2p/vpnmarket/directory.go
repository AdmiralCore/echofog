package vpnmarket

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	maxCandidates   = 300
	lookupWorkers   = 16
	lookupTimeout   = 4 * time.Second
	neighborsToShow = 6
	maxCrawlRounds  = 3
)

// DHTClient is the slice of the DHT the market needs
type DHTClient interface {
	Set(key, value []byte) error
	GetFresh(key []byte) ([]byte, error)
	KnownPeers() []types.PeerID
}

// Entry is one exit offer joined with the local rating of this client
type Entry struct {
	Advert Advert
	Rating Rating
	Score  float64
}

// Directory finds exit adverts in the DHT, verifies them and keeps the local ratings
type Directory struct {
	dht     DHTClient
	ratings *RatingStore
	self    types.PeerID
	peers   func() []types.PeerID
	now     func() time.Time

	mu      sync.RWMutex
	adverts map[types.PeerID]*Advert
}

func NewDirectory(dht DHTClient, ratings *RatingStore, self types.PeerID, connected func() []types.PeerID) *Directory {
	return &Directory{dht: dht, ratings: ratings, self: self, peers: connected, now: time.Now, adverts: make(map[types.PeerID]*Advert)}
}

func (d *Directory) candidates() []types.PeerID {
	seen := map[types.PeerID]bool{d.self: true}
	var out []types.PeerID
	add := func(id types.PeerID) {
		if !seen[id] && len(out) < maxCandidates {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range d.dht.KnownPeers() {
		add(id)
	}
	if d.peers != nil {
		for _, id := range d.peers() {
			add(id)
		}
	}
	for _, r := range d.ratings.All() {
		if id, err := parseID(r.PeerID); err == nil {
			add(id)
		}
	}
	return out
}

func parseID(s string) (types.PeerID, error) {
	var id types.PeerID
	err := id.UnmarshalText([]byte(s))
	return id, err
}

// lookup fetches and verifies one advert; the DHT key binds it to the candidate, so a mismatch means spoofing
func (d *Directory) lookup(peer types.PeerID) *Advert {
	raw, err := d.dht.GetFresh(Key(peer))
	if err != nil {
		return nil
	}
	a, err := Decode(raw, d.now())
	if err != nil {
		return nil
	}
	if id, err := a.ID(); err != nil || id != peer {
		return nil
	}
	return a
}

func (d *Directory) lookupAll(ctx context.Context, peers []types.PeerID) []*Advert {
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		out []*Advert
		sem = make(chan struct{}, lookupWorkers)
	)
	for _, p := range peers {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(p types.PeerID) {
			defer func() { <-sem; wg.Done() }()
			c, cancel := context.WithTimeout(ctx, lookupTimeout)
			defer cancel()
			done := make(chan *Advert, 1)
			go func() { done <- d.lookup(p) }()
			select {
			case a := <-done:
				if a != nil {
					mu.Lock()
					out = append(out, a)
					mu.Unlock()
				}
			case <-c.Done():
			}
		}(p)
	}
	wg.Wait()
	return out
}

func (d *Directory) accept(a *Advert) {
	id, err := a.ID()
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if old, ok := d.adverts[id]; ok && old.IssuedAt > a.IssuedAt {
		return
	}
	if !a.Enabled {
		delete(d.adverts, id)
	} else {
		d.adverts[id] = a
	}
	_ = d.ratings.RecordAdvert(id, a, d.now())
}

// Refresh scans known peers and the neighbours they advertise; it returns the number of live exits
func (d *Directory) Refresh(ctx context.Context) int {
	known := map[types.PeerID]bool{d.self: true}
	batch := d.candidates()
	for _, p := range batch {
		known[p] = true
	}

	for round := 0; round < maxCrawlRounds && len(batch) > 0 && ctx.Err() == nil; round++ {
		found := d.lookupAll(ctx, batch)
		batch = batch[:0:0]
		for _, a := range found {
			d.accept(a)
			for _, n := range a.Neighbors {
				if id, err := parseID(n); err == nil && !known[id] && len(known) < maxCandidates*2 {
					known[id] = true
					batch = append(batch, id)
				}
			}
		}
	}

	d.expire()
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.adverts)
}

func (d *Directory) expire() {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, a := range d.adverts {
		if !time.Unix(a.ExpiresAt, 0).After(now) {
			delete(d.adverts, id)
		}
	}
}

// List returns live exits sorted by score, best first
func (d *Directory) List() []Entry {
	d.expire()
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Entry, 0, len(d.adverts))
	for id, a := range d.adverts {
		r, ok := d.ratings.Get(id)
		if !ok {
			r = Rating{PeerID: id.String()}
		}
		out = append(out, Entry{Advert: *a, Rating: r, Score: Score(r, a)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Advert.PeerID < out[j].Advert.PeerID
	})
	return out
}

// TopNeighbors lists the best known exit IDs for embedding in our own advert
func (d *Directory) TopNeighbors() []string {
	var out []string
	for _, e := range d.List() {
		if len(out) >= neighborsToShow {
			break
		}
		out = append(out, e.Advert.PeerID)
	}
	return out
}

// Get returns one live exit
func (d *Directory) Get(peer types.PeerID) (Entry, bool) {
	for _, e := range d.List() {
		if e.Advert.PeerID == peer.String() {
			return e, true
		}
	}
	return Entry{}, false
}
