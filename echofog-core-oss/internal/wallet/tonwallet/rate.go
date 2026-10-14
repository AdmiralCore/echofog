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
	usd float64
	at  time.Time
}

// USDRate returns the TON/USD price, cached 10 minutes. It values TON payments for traffic: the
// payer adds a margin, and the exit credits at the rate when the payment arrives.
func USDRate(ctx context.Context) (float64, error) {
	rateCache.Lock()
	defer rateCache.Unlock()
	if rateCache.usd > 0 && time.Since(rateCache.at) < 10*time.Minute {
		return rateCache.usd, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rateURL, nil)
	if err != nil {
		return 0, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return rateCache.usd, err
	}
	defer res.Body.Close()
	var body struct {
		Rates map[string]struct {
			Prices map[string]float64 `json:"prices"`
		} `json:"rates"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return rateCache.usd, err
	}
	usd := body.Rates["TON"].Prices["USD"]
	if usd <= 0 {
		return rateCache.usd, fmt.Errorf("no TON/USD rate in response")
	}
	rateCache.usd, rateCache.at = usd, time.Now()
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
