package vpnmarket

import (
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const ratingPrefix = "vpnmarket:rating:"

// Rating is this client's own local view of one exit node; it is never published
type Rating struct {
	PeerID           string  `json:"peer"`
	FirstSeen        int64   `json:"first_seen"`
	LastSeen         int64   `json:"last_seen"`
	LastProbe        int64   `json:"last_probe"`
	Probes           int     `json:"probes"`
	Successes        int     `json:"successes"`
	ConsecutiveFails int     `json:"consecutive_fails"`
	LatencyMs        float64 `json:"latency_ms"`
	MeasuredKbps     int     `json:"measured_kbps"`
	ClaimedKbps      int     `json:"claimed_kbps"`
	DomainsTried     int     `json:"domains_tried"`
	DomainsOK        int     `json:"domains_ok"`
	PaymentRequired  bool    `json:"payment_required"`
	LastAdvert       *Advert `json:"last_advert,omitempty"`
}

// ProbeResult is the outcome of one availability and speed check
type ProbeResult struct {
	At              time.Time
	OK              bool
	LatencyMs       float64
	MeasuredKbps    int
	DomainsTried    int
	DomainsOK       int
	PaymentRequired bool
}

// RatingStore persists ratings in the node database
type RatingStore struct {
	mu sync.Mutex
	st storage.Storage
}

func NewRatingStore(st storage.Storage) *RatingStore { return &RatingStore{st: st} }

func ratingKey(peer types.PeerID) []byte { return []byte(ratingPrefix + peer.String()) }

func (s *RatingStore) get(peer types.PeerID) *Rating {
	raw, err := s.st.Get(ratingKey(peer))
	if err != nil || len(raw) == 0 {
		return nil
	}
	var r Rating
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	return &r
}

func (s *RatingStore) put(peer types.PeerID, r *Rating) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.st.Set(ratingKey(peer), raw)
}

// Get returns the stored rating for a peer
func (s *RatingStore) Get(peer types.PeerID) (Rating, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.get(peer); r != nil {
		return *r, true
	}
	return Rating{}, false
}

// All returns every stored rating
func (s *RatingStore) All() []Rating {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.st.FindValues([]byte(ratingPrefix), 5000)
	if err != nil {
		return nil
	}
	out := make([]Rating, 0, len(entries))
	for _, e := range entries {
		var r Rating
		if json.Unmarshal(e.Value, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// RecordAdvert notes that a verified advert was seen
func (s *RatingStore) RecordAdvert(peer types.PeerID, a *Advert, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(peer)
	if r == nil {
		r = &Rating{PeerID: peer.String(), FirstSeen: now.Unix()}
	}
	r.LastSeen = now.Unix()
	r.ClaimedKbps = a.UploadKbps
	r.LastAdvert = a
	return s.put(peer, r)
}

// RecordProbe folds a probe result into the running statistics
func (s *RatingStore) RecordProbe(peer types.PeerID, res ProbeResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(peer)
	if r == nil {
		r = &Rating{PeerID: peer.String(), FirstSeen: res.At.Unix()}
	}
	r.LastProbe = res.At.Unix()
	r.Probes++
	r.PaymentRequired = res.PaymentRequired
	if res.OK {
		r.Successes++
		r.ConsecutiveFails = 0
		if r.LatencyMs == 0 {
			r.LatencyMs = res.LatencyMs
		} else if res.LatencyMs > 0 {
			r.LatencyMs = 0.7*r.LatencyMs + 0.3*res.LatencyMs
		}
	} else {
		r.ConsecutiveFails++
	}
	if res.MeasuredKbps > 0 {
		if r.MeasuredKbps == 0 {
			r.MeasuredKbps = res.MeasuredKbps
		} else {
			r.MeasuredKbps = int(0.6*float64(r.MeasuredKbps) + 0.4*float64(res.MeasuredKbps))
		}
	}
	r.DomainsTried += res.DomainsTried
	r.DomainsOK += res.DomainsOK
	return s.put(peer, r)
}

// Score ranks an exit from 0 to 1: reliability, honesty of the claimed bandwidth, latency, reachable domains and price
func Score(r Rating, a *Advert) float64 {
	reliability := (float64(r.Successes) + 1) / (float64(r.Probes) + 2)

	honesty := 0.7
	if r.ClaimedKbps > 0 && r.MeasuredKbps > 0 {
		honesty = math.Min(1, float64(r.MeasuredKbps)/float64(r.ClaimedKbps))
	}

	latency := 0.8
	if r.LatencyMs > 0 {
		latency = 1 / (1 + r.LatencyMs/300)
	}

	domains := 0.7
	if r.DomainsTried > 0 {
		domains = (float64(r.DomainsOK) + 1) / (float64(r.DomainsTried) + 2)
	}

	price := 1.0
	if a != nil && a.PricePerMB > 0 {
		price = 1 / (1 + float64(a.PricePerMB)/500)
	}

	s := reliability * honesty * latency * domains * price
	if r.ConsecutiveFails >= 3 {
		s *= 0.2
	}
	return s
}
