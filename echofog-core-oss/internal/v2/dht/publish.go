package dht

import (
	"context"
	"math/rand"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Republication (dht.md §2, §3). A record is stored under the key of the current day, and in the
// last 2 hours of a day under the next day's key too; readers look under the previous day's key in
// the first 2 hours. So that both keys stay filled the whole time, a record is stored again inside
// a window of ± 2 hours around every midnight. It is also stored when half of its remaining
// lifetime has passed, whichever is earlier, each with ± 20 % jitter (AD13).

const (
	publishWindow = 2 * time.Hour
	day           = 24 * time.Hour
	// minRepublish is the shortest delay and the lifetime below which a record is not stored
	// again: near expiry halving would never end.
	minRepublish = 10 * time.Minute
	// retryAfter is the delay after a publication that stored nowhere.
	retryAfter = 5 * time.Minute
)

type published struct {
	raw    []byte
	env    *objects.Envelope
	next   time.Time
	failed int
}

// halfLifeDelay is half of the remaining lifetime with ± 20 % jitter.
func halfLifeDelay(remaining time.Duration, rnd *rand.Rand) time.Duration {
	return time.Duration(float64(remaining/2) * (0.8 + 0.4*rnd.Float64()))
}

// nextPublish returns when to store a record again: the earlier of half its remaining lifetime
// and a random moment in the window around the next midnight that lies at least until the
// window opens. ok is false when the record is too close to its end to be stored again.
func nextPublish(now time.Time, notAfter time.Time, rnd *rand.Rand) (time.Time, bool) {
	remaining := notAfter.Sub(now)
	if remaining/2 < minRepublish {
		return time.Time{}, false
	}
	at := now.Add(halfLifeDelay(remaining, rnd))

	midnight := now.UTC().Truncate(day).Add(day)
	if now.Add(time.Minute).After(midnight.Add(-publishWindow)) {
		midnight = midnight.Add(day)
	}
	window := midnight.Add(-publishWindow).Add(time.Duration(rnd.Int63n(int64(2 * publishWindow))))
	if window.Before(at) {
		at = window
	}
	if min := now.Add(minRepublish); at.Before(min) {
		at = min
	}
	if !at.Before(notAfter) {
		return time.Time{}, false
	}
	return at, true
}

func pubKey(env *objects.Envelope) string {
	id := env.SignerID()
	return env.Label + "\x00" + string(id[:])
}

// Publish registers a record of this node for storing and for republication; Step and Run do the
// work. A record with a higher seq replaces the registered one of the same (label, signer). The
// first publication happens at the next Step.
func (c *Client) Publish(record []byte) error {
	now := c.now()
	env, err := objects.Verify(record, now)
	if err != nil {
		return err
	}
	if !storable(env.Label) {
		return ErrNotStorable
	}
	if err := checkPublishable(env, now); err != nil {
		return err
	}
	c.pubMu.Lock()
	defer c.pubMu.Unlock()
	k := pubKey(env)
	if old := c.pubs[k]; old != nil && old.env.Seq >= env.Seq {
		return nil
	}
	c.pubs[k] = &published{raw: record, env: env, next: now}
	return nil
}

// Step stores every registered record that is due and drops the ones that have expired.
func (c *Client) Step(ctx context.Context) {
	if c.cfg.Own != nil {
		for _, rec := range c.cfg.Own() {
			if err := c.Publish(rec); err != nil {
				c.log.Debug("dht: own record", "err", err)
			}
		}
	}
	now := c.now()
	c.pubMu.Lock()
	var due []*published
	for k, p := range c.pubs {
		if !time.Unix(int64(p.env.NotAfter), 0).After(now) {
			delete(c.pubs, k)
			continue
		}
		if !p.next.After(now) {
			due = append(due, p)
		}
	}
	c.pubMu.Unlock()

	for _, p := range due {
		if ctx.Err() != nil {
			return
		}
		res, err := c.Put(ctx, p.raw)
		now = c.now()
		notAfter := time.Unix(int64(p.env.NotAfter), 0)

		c.rndMu.Lock()
		at, ok := nextPublish(now, notAfter, c.rnd)
		c.rndMu.Unlock()
		c.pubMu.Lock()
		if err != nil {
			p.failed++
			c.log.Debug("dht: publication", "label", p.env.Label, "stored", res.Stored, "err", err)
			if retry := now.Add(time.Duration(float64(retryAfter) * (0.8 + 0.4*c.random()))); ok && retry.Before(at) {
				at = retry
			}
		} else {
			p.failed = 0
		}
		if !ok {
			delete(c.pubs, pubKey(p.env))
		} else {
			p.next = at
		}
		c.pubMu.Unlock()
	}
}

// Run calls Step once a minute until ctx ends.
func (c *Client) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		c.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
