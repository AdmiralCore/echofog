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
	if !policy.Enabled || policy.MaxCircuits != config.DefaultRelayMaxCircuits {
		t.Fatalf("expected default enabled=true max_circuits=200, got %+v", policy)
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

func mkID(b byte) types.PeerID { var id types.PeerID; id[0] = b; return id }

func TestRelayServer_PrivateAccess(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil)
	allowed, stranger := mkID(1), mkID(2)

	// public (default): everyone is served
	if !rs.servesPeer(stranger) {
		t.Fatal("a public relay must serve any peer")
	}

	rs.SetPolicy(config.RelayServerConfig{
		Enabled:      true,
		Private:      true,
		AllowedPeers: []string{allowed.String(), "not-a-peer-id"},
	})
	if got := rs.Stats().AllowedPeers; got != 1 {
		t.Fatalf("expected 1 valid allowed peer (the malformed one dropped), got %d", got)
	}
	if !rs.servesPeer(allowed) {
		t.Fatal("a private relay must serve an allowed peer")
	}
	if rs.servesPeer(stranger) {
		t.Fatal("a private relay must not serve a peer that is not on the allow list")
	}
	if !rs.Stats().Private {
		t.Fatal("Stats should report the relay as private")
	}
}

func TestRelayClient_PreferredRelays(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	pref, other := mkID(7), mkID(8)

	rc.SetPolicy(config.RelayClientConfig{
		Enabled: true,
		Relays:  []string{"1.2.3.4:443/" + pref.String(), "garbage"},
	})
	if !rc.IsPreferred(pref) {
		t.Fatal("a relay named in the config must be preferred")
	}
	if rc.IsPreferred(other) {
		t.Fatal("a relay not in the config must not be preferred")
	}
	// a bare peer id is also accepted
	rc.SetPolicy(config.RelayClientConfig{Enabled: true, Relays: []string{other.String()}})
	if !rc.IsPreferred(other) {
		t.Fatal("a bare peer id in the config must be preferred")
	}
	if rc.IsPreferred(pref) {
		t.Fatal("the previous preferred relay should be gone after a new policy")
	}
}
