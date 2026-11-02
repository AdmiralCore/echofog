package routing

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
)

// The daily relay budget starts again on a new UTC day: before, the counter never reset and
// every paid relay stayed "not viable" until a restart once the limit was reached.
func TestDailyBudgetResetsNextDay(t *testing.T) {
	day := time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC)
	rc := &RelayClient{cfg: config.RelayClientConfig{DailyBudgetLimit: 1000}}
	rc.now = func() time.Time { return day }

	rc.RecordRelayExpense(1500)
	if got := rc.GetDailySpent(); got != 1500 {
		t.Fatalf("spent today = %d, want 1500", got)
	}
	day = day.Add(2 * time.Hour) // past midnight UTC
	if got := rc.GetDailySpent(); got != 0 {
		t.Fatalf("spent after midnight = %d, want 0", got)
	}
	rc.RecordRelayExpense(200)
	if got := rc.GetDailySpent(); got != 200 {
		t.Fatalf("spent on the new day = %d, want 200", got)
	}
}
