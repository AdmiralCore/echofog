package tonwallet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func resetRateCache(t *testing.T, url string) {
	prev := rateURL
	rateURL = url
	rateCache.Lock()
	rateCache.usd, rateCache.at, rateCache.failedAt, rateCache.lastErr = 0, time.Time{}, time.Time{}, nil
	rateCache.Unlock()
	t.Cleanup(func() {
		rateURL = prev
		rateCache.Lock()
		rateCache.usd, rateCache.at, rateCache.failedAt, rateCache.lastErr = 0, time.Time{}, time.Time{}, nil
		rateCache.Unlock()
	})
}

// A rate API that is down or blocked must not make every payment round wait on it again.
func TestUSDRateBacksOffAfterAFailure(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer srv.Close()
	resetRateCache(t, srv.URL)

	if _, err := USDRate(context.Background()); err == nil {
		t.Fatal("a failed fetch must report its error")
	}
	for i := 0; i < 5; i++ {
		if _, err := USDRate(context.Background()); err == nil {
			t.Fatal("the remembered failure is reported")
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the API was asked %d times, want once until rateRetryAfter passes", n)
	}
}

// The UI's cached read never waits for a fetch in progress.
func TestCachedUSDRateDoesNotWaitForTheNetwork(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"rates":{"TON":{"prices":{"USD":3.2}}}}`))
	}))
	defer srv.Close()
	defer close(release)
	resetRateCache(t, srv.URL)

	go func() { _, _ = USDRate(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // the fetch is now waiting on the server

	done := make(chan float64, 1)
	go func() { done <- CachedUSDRate() }()
	select {
	case v := <-done:
		if v != 0 {
			t.Fatalf("nothing is cached yet, got %v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("CachedUSDRate blocked on a fetch in progress")
	}
}
