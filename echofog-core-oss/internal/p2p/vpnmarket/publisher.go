package vpnmarket

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// ShareConfig is what an operator offers
type ShareConfig struct {
	PricePerMB int64 `json:"pricePerMb"`
	UploadKbps int   `json:"uploadKbps"`
	FreeTestKB int   `json:"freeTestKb"`
}

// PublishStatus reports the state of the background publisher
type PublishStatus struct {
	Running     bool        `json:"running"`
	Config      ShareConfig `json:"config"`
	LastPublish int64       `json:"lastPublish"`
	ExpiresAt   int64       `json:"expiresAt"`
	LastError   string      `json:"lastError"`
}

// Publisher keeps a signed advert alive in the DHT while sharing is on
type Publisher struct {
	dht       DHTClient
	signer    Signer
	self      types.PeerID
	pub       types.PeerPublicKey
	neighbors func() []string
	sni       string
	relayFee  func() int64 // what this node asks for relaying, advertised with its offer
	now       func() time.Time
	interval  time.Duration

	// pubMu makes each publish (read the offer, sign, store) one step, so a timed republish that
	// read the old offer cannot land after an Update or a withdrawal
	pubMu  sync.Mutex
	mu     sync.Mutex
	status PublishStatus
	cancel context.CancelFunc
	last   []byte // the advert last stored while sharing, nil when withdrawn
}

func NewPublisher(dht DHTClient, signer Signer, self types.PeerID, pub types.PeerPublicKey, neighbors func() []string) *Publisher {
	return &Publisher{dht: dht, signer: signer, self: self, pub: pub, neighbors: neighbors, now: time.Now, interval: RepublishInterval}
}

// SetSNI sets the camouflage SNI advertised with this node's exit offer.
func (p *Publisher) SetSNI(sni string) {
	p.mu.Lock()
	p.sni = sni
	p.mu.Unlock()
}

// SetRelayPrice sets where the price this node asks for relaying circuits is read from when the
// advert is made.
func (p *Publisher) SetRelayPrice(fee func() int64) {
	p.mu.Lock()
	p.relayFee = fee
	p.mu.Unlock()
}

func (c ShareConfig) validate() error {
	switch {
	case c.PricePerMB < 0 || c.PricePerMB > MaxPricePerMB:
		return errors.New("price is out of range")
	case c.FreeTestKB < 0 || c.FreeTestKB > MaxFreeTestKB:
		return errors.New("free test traffic must be between 0 and 1024 KB")
	case c.UploadKbps < 0 || c.UploadKbps > MaxUploadKbps:
		return errors.New("upload speed is out of range")
	}
	return nil
}

func (p *Publisher) build(enabled bool, cfg ShareConfig) (*Advert, error) {
	now := p.now()
	a := &Advert{
		Version:    AdvertVersion,
		PeerID:     p.self.String(),
		PubKey:     p.pub[:],
		Enabled:    enabled,
		PricePerMB: cfg.PricePerMB,
		UploadKbps: cfg.UploadKbps,
		FreeTestKB: cfg.FreeTestKB,
		SNI:        p.sni,
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(AdvertTTL).Unix(),
	}
	if p.relayFee != nil {
		if fee := p.relayFee(); fee > 0 && fee <= MaxPricePerMB {
			a.RelayPricePerMB = fee
		}
	}
	if enabled && p.neighbors != nil {
		a.Neighbors = p.neighbors()
		if len(a.Neighbors) > MaxNeighbors {
			a.Neighbors = a.Neighbors[:MaxNeighbors]
		}
	}
	if err := a.Sign(p.signer); err != nil {
		return nil, err
	}
	return a, nil
}

func (p *Publisher) publish(enabled bool) error {
	p.pubMu.Lock()
	defer p.pubMu.Unlock()
	p.mu.Lock()
	cfg, running := p.status.Config, p.status.Running
	p.mu.Unlock()
	if enabled && !running {
		return nil // stopped meanwhile: the withdrawal stands
	}

	a, err := p.build(enabled, cfg)
	if err == nil {
		var raw []byte
		if raw, err = a.Encode(); err == nil {
			if enabled {
				p.mu.Lock()
				p.last = raw // handed out directly too, whether the DHT took it or not
				p.mu.Unlock()
			}
			err = p.dht.Set(Key(p.self), raw)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if !enabled {
		p.last = nil
	}
	if err != nil {
		p.status.LastError = err.Error()
		return err
	}
	p.status.LastError = ""
	p.status.LastPublish = a.IssuedAt
	p.status.ExpiresAt = a.ExpiresAt
	return nil
}

// Start publishes immediately and then republishes on an interval until Stop
func (p *Publisher) Start(parent context.Context, cfg ShareConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	p.status.Config = cfg
	p.status.Running = true
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	p.mu.Unlock()

	if err := p.publish(true); err != nil {
		p.mu.Lock()
		cancel()
		p.cancel = nil
		p.status.Running = false
		p.mu.Unlock()
		return err
	}

	go func() {
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = p.publish(true)
			}
		}
	}()
	return nil
}

// Update changes the offer while sharing and republishes at once
func (p *Publisher) Update(cfg ShareConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	p.mu.Lock()
	running := p.status.Running
	p.status.Config = cfg
	p.mu.Unlock()
	if !running {
		return nil
	}
	return p.publish(true)
}

// Stop ends the loop and publishes a signed withdrawal so clients drop the node before the TTL ends
func (p *Publisher) Stop() error {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	wasRunning := p.status.Running
	p.status.Running = false
	p.mu.Unlock()
	if !wasRunning {
		return nil
	}
	return p.publish(false)
}

// Current returns this node's own advert while it shares its connection publicly, nil otherwise.
// Peers asking for the exits we know get it first: a client connected to us finds us even when
// its DHT lookups reach only stale nodes.
func (p *Publisher) Current() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.status.Running || p.last == nil {
		return nil
	}
	if a, err := Decode(p.last, p.now()); err != nil || a == nil {
		return nil // expired: the next republish brings a fresh one
	}
	return p.last
}

func (p *Publisher) Status() PublishStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}
