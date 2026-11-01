package tonwallet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

var rateURL = "https://tonapi.io/v2/rates?tokens=ton&currencies=usd"

var rateCache struct {
	sync.Mutex
	usd      float64
	at       time.Time
	failedAt time.Time // the last failed fetch: callers are not made to wait on it again for a while
	lastErr  error
}

// rateFetch lets one fetch run at a time; the cache lock is never held while it waits, so
// CachedUSDRate (read by the UI every few seconds) never blocks on the network.
var rateFetch sync.Mutex

// rateRetryAfter: after a failed fetch (the rate API is down or blocked), the cached value is
// returned at once for this long instead of every caller waiting up to 5 s again.
const rateRetryAfter = time.Minute

// USDRate returns the TON/USD price, cached 10 minutes. It values TON payments for traffic: the
// payer adds a margin, and the exit credits at the rate when the payment arrives.
func USDRate(ctx context.Context) (float64, error) {
	if usd, ok, err := cachedRate(); ok {
		return usd, err
	}
	rateFetch.Lock()
	defer rateFetch.Unlock()
	if usd, ok, err := cachedRate(); ok { // fetched while we waited
		return usd, err
	}
	usd, err := fetchUSDRate(ctx)
	rateCache.Lock()
	defer rateCache.Unlock()
	if err != nil {
		rateCache.failedAt, rateCache.lastErr = time.Now(), err
		return rateCache.usd, err
	}
	rateCache.usd, rateCache.at, rateCache.failedAt, rateCache.lastErr = usd, time.Now(), time.Time{}, nil
	return usd, nil
}

// cachedRate answers from the cache when it is fresh, or when a fetch failed a moment ago.
func cachedRate() (float64, bool, error) {
	rateCache.Lock()
	defer rateCache.Unlock()
	if rateCache.usd > 0 && time.Since(rateCache.at) < 10*time.Minute {
		return rateCache.usd, true, nil
	}
	if !rateCache.failedAt.IsZero() && time.Since(rateCache.failedAt) < rateRetryAfter {
		return rateCache.usd, true, rateCache.lastErr
	}
	return 0, false, nil
}

func fetchUSDRate(ctx context.Context) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rateURL, nil)
	if err != nil {
		return 0, err
	}
	c := client()
	if c == nil {
		return 0, errNoHTTP
	}
	res, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	var body struct {
		Rates map[string]struct {
			Prices map[string]float64 `json:"prices"`
		} `json:"rates"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return 0, err
	}
	usd := body.Rates["TON"].Prices["USD"]
	if usd <= 0 {
		return 0, fmt.Errorf("no TON/USD rate in response")
	}
	return usd, nil
}

var rateRefreshing struct {
	sync.Mutex
	on bool
}

// CachedUSDRate returns the last known TON/USD price without waiting for the network (0 when
// none is known or it is over an hour old) and refreshes it in the background when it is stale.
func CachedUSDRate() float64 {
	rateCache.Lock()
	usd, at := rateCache.usd, rateCache.at
	rateCache.Unlock()
	if usd <= 0 || time.Since(at) > 10*time.Minute {
		rateRefreshing.Lock()
		if !rateRefreshing.on {
			rateRefreshing.on = true
			go func() {
				_, _ = USDRate(context.Background())
				rateRefreshing.Lock()
				rateRefreshing.on = false
				rateRefreshing.Unlock()
			}()
		}
		rateRefreshing.Unlock()
	}
	if usd <= 0 || time.Since(at) > time.Hour {
		return 0
	}
	return usd
}
