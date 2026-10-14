package routing

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func TestRelayServer_PolicyAndLimits(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil)

	// Check default policy
	policy := rs.GetPolicy()
	if !policy.Enabled || policy.MaxCircuits != 50 {
		t.Fatalf("expected default enabled=true max_circuits=50, got %+v", policy)
	}

	// Update policy
	rs.SetPolicy(config.RelayServerConfig{
		Enabled:     false,
		PricePerMB:  250,
		MaxCircuits: 10,
	})

	updated := rs.GetPolicy()
	if updated.Enabled || updated.PricePerMB != 250 || updated.MaxCircuits != 10 {
		t.Fatalf("policy not updated properly: %+v", updated)
	}
}

func TestRelayClient_ScoringAndMarket(t *testing.T) {
	rc := &RelayClient{
		relayPrices: make(map[types.PeerID]int64),
		relayPings:  make(map[types.PeerID]time.Duration),
		cfg: config.RelayClientConfig{
			Enabled:                 true,
			MaxAcceptablePricePerMB: 200,
			MaxAcceptablePingMs:     300,
			DailyBudgetLimit:        1000,
			MarketAwareSelection:    true,
		},
	}

	// Test Scoring
	// Fast & Cheap: ping 10ms, price 5
	scoreFastCheap := rc.ScoreRelay(5, 10*time.Millisecond)

	// Slow & Expensive: ping 250ms, price 150
	scoreSlowExpensive := rc.ScoreRelay(150, 250*time.Millisecond)

	if scoreFastCheap <= scoreSlowExpensive {
		t.Fatalf("expected fast/cheap relay score (%f) to be significantly higher than slow/expensive (%f)",
			scoreFastCheap, scoreSlowExpensive)
	}

	// Test Budget Tracking
	if rc.GetDailySpent() != 0 {
		t.Fatalf("expected initial daily spent to be 0")
	}

	rc.RecordRelayExpense(500)
	if rc.GetDailySpent() != 500 {
		t.Fatalf("expected daily spent=500, got %d", rc.GetDailySpent())
	}

	rc.RecordRelayExpense(600) // now 1100, exceeding limit 1000
	if rc.GetDailySpent() != 1100 {
		t.Fatalf("expected daily spent=1100, got %d", rc.GetDailySpent())
	}

	rc.ResetDailySpent()
	if rc.GetDailySpent() != 0 {
		t.Fatalf("expected reset daily spent=0, got %d", rc.GetDailySpent())
	}
}
