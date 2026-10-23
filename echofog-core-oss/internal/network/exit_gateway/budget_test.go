package exit_gateway

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// budgetExit is a free exit with the given caps and peers at the addresses in addr.
func budgetExit(t *testing.T, l Limits) (*ExitService, map[types.PeerID]string) {
	t.Helper()
	svc := NewExitService()
	svc.SetEnabled(true)
	svc.SetLimits(l)
	addr := map[types.PeerID]string{}
	var mu sync.Mutex
	svc.SetAddrOf(func(id types.PeerID) string { mu.Lock(); defer mu.Unlock(); return addr[id] })
	return svc, addr
}

// spendAll spends chunks of n bytes through one meter until it is cut, and returns what passed.
func spendAll(svc *ExitService, peer types.PeerID, n int) int64 {
	m := newMeter(svc, peer)
	defer m.close()
	var spent int64
	for i := 0; i < 1<<16 && m.spend(n); i++ {
		spent += int64(n)
	}
	return spent
}

func TestFreeBudgetFromConfig(t *testing.T) {
	d := DefaultLimits()
	assert.Zero(t, d.FreePeerDaily, "no traffic caps unless the operator sets them")
	assert.Zero(t, d.FreeIPDaily)
	assert.Zero(t, d.FreeMonthly)
	l := LimitsFromConfig(config.ExitConfig{FreePeerMBPerDay: 2, FreeIPMBPerDay: -1, FreeGBPerMonth: 3})
	assert.Equal(t, int64(2<<20), l.FreePeerDaily)
	assert.Zero(t, l.FreeIPDaily, "negative: no cap")
	assert.Equal(t, int64(3<<30), l.FreeMonthly)
}

// Without caps a free exit stays unmetered, as before.
func TestFreeExitWithoutCapsIsUnlimited(t *testing.T) {
	svc, _ := budgetExit(t, DefaultLimits())
	assert.Equal(t, int64(-1), svc.allowance(testClient))
	assert.Equal(t, int64(1<<16)*1024, spendAll(svc, testClient, 1024))
	used, limit := svc.FreeMonth()
	assert.Zero(t, used, "nothing is counted when no cap is set")
	assert.Zero(t, limit)
}

// One peer gets its daily share and no more, the next peer still gets its own, and a refused
// stream is reported as the free limit, not as a payment.
func TestFreeBudgetPerPeerDaily(t *testing.T) {
	l := DefaultLimits()
	l.FreePeerDaily = 1 << 20
	svc, _ := budgetExit(t, l)

	assert.Equal(t, int64(1<<20), spendAll(svc, testClient, 32<<10))
	assert.Zero(t, svc.allowance(testClient))
	assert.Equal(t, int64(1<<20), spendAll(svc, types.PeerID{2}, 32<<10), "another peer has its own share")

	svc.allowPrivate.Store(true)
	conn, err := NewP2PDialer(serve(svc)).DialExitStreamConfirmed(context.Background(), echoServer(t))
	require.NoError(t, err, "the zero peer of HandleTunnel has its own share left")
	conn.Close()
	used, _ := svc.FreeMonth()
	assert.Equal(t, int64(2<<20), used, "the month total counts every peer's free traffic")

	assert.Equal(t, RefuseFreeLimit, svc.paymentRefusal())
	svc.SetPolicy(Policy{PricePerMB: 49})
	assert.Equal(t, RefusePayment, svc.paymentRefusal(), "a priced exit asks for payment")
}

// A stream that hits the cap mid-way is cut, and the client sees why.
func TestFreeBudgetCutsAStream(t *testing.T) {
	l := DefaultLimits()
	l.FreePeerDaily = 256 << 10
	svc, _ := budgetExit(t, l)
	svc.allowPrivate.Store(true)
	d := NewP2PDialer(serve(svc))

	conn, err := d.DialExitStreamConfirmed(context.Background(), echoServer(t))
	require.NoError(t, err)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			if _, err := conn.Write(buf); err != nil {
				return
			}
		}
	}()
	n, _ := io.Copy(io.Discard, conn)
	conn.Close()
	assert.LessOrEqual(t, n, int64(256<<10), "no more comes back than the cap")

	_, err = d.DialExitStreamConfirmed(context.Background(), echoServer(t))
	assert.ErrorIs(t, err, ErrPaymentRequired)
	assert.Equal(t, uint64(1), svc.RefusedByReason()[RefuseFreeLimit])
	assert.Zero(t, svc.RefusedByReason()[RefusePayment])
}

// Identities of one public address share its cap; a LAN address is not counted per address.
func TestFreeBudgetPerIP(t *testing.T) {
	l := DefaultLimits()
	l.FreeIPDaily = 1 << 20
	svc, addr := budgetExit(t, l)
	a, b, lan := types.PeerID{1}, types.PeerID{2}, types.PeerID{3}
	addr[a], addr[b], addr[lan] = "203.0.113.8:1", "203.0.113.8:2", "192.168.1.5:3"

	got := spendAll(svc, a, 32<<10)
	assert.Equal(t, int64(1<<20), got)
	assert.Zero(t, spendAll(svc, b, 32<<10), "a fresh identity behind the same address gets nothing more")
	assert.Equal(t, int64(1<<16)*(32<<10), spendAll(svc, lan, 32<<10), "no per-address cap for a LAN address")
}

// The month cap is shared by everybody, survives a restart and starts over next month.
func TestFreeBudgetMonthly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit-free-budget.json")
	l := DefaultLimits()
	l.FreeMonthly = 1 << 20
	svc, _ := budgetExit(t, l)
	svc.SetFreeBudgetFile(path)

	m := newMeter(svc, types.PeerID{1})
	require.True(t, m.spend(512<<10))
	m.close()
	assert.Equal(t, int64(512<<10), spendAll(svc, types.PeerID{2}, 32<<10), "the rest of the month for the next peer")
	assert.Zero(t, svc.allowance(types.PeerID{3}), "the month is used up for everybody")
	svc.FlushFreeBudget()

	again, _ := budgetExit(t, l)
	again.SetFreeBudgetFile(path)
	assert.Zero(t, again.allowance(types.PeerID{3}), "a restart does not give the month away again")
	used, limit := again.FreeMonth()
	assert.Equal(t, int64(1<<20), used)
	assert.Equal(t, int64(1<<20), limit)

	again.budget.mu.Lock()
	again.budget.month = again.budget.month.AddDate(0, -1, 0)
	again.budget.mu.Unlock()
	assert.Equal(t, int64(1<<20), again.allowance(types.PeerID{3}), "a new month starts with the whole budget")
}

// The daily counters start over on a new day; the month total does not.
func TestFreeBudgetNewDay(t *testing.T) {
	l := DefaultLimits()
	l.FreePeerDaily, l.FreeMonthly = 1<<20, 10<<20
	svc, _ := budgetExit(t, l)
	spendAll(svc, testClient, 32<<10)
	assert.Zero(t, svc.allowance(testClient))

	svc.budget.mu.Lock()
	svc.budget.day = svc.budget.day.Add(-24 * time.Hour)
	svc.budget.mu.Unlock()
	assert.Equal(t, int64(1<<20), svc.allowance(testClient))
	used, _ := svc.FreeMonth()
	assert.Equal(t, int64(1<<20), used, "yesterday's traffic still counts this month")
}

// The operator's own devices and paying clients never count against the free budget.
func TestFreeBudgetSkipsFreeListAndPaidExit(t *testing.T) {
	l := DefaultLimits()
	l.FreePeerDaily, l.FreeMonthly = 64<<10, 64<<10
	svc, _ := budgetExit(t, l)
	mine := types.PeerID{3}
	svc.SetFreePeers([]types.PeerID{mine})
	assert.Equal(t, int64(-1), svc.allowance(mine))
	assert.Equal(t, int64(1<<16)*1024, spendAll(svc, mine, 1024))
	used, _ := svc.FreeMonth()
	assert.Zero(t, used)

	svc.SetPolicy(Policy{PricePerMB: 49, FreeBytes: 1000})
	assert.Equal(t, int64(1000), svc.allowance(testClient), "a priced exit uses its own test quota")
	used, _ = svc.FreeMonth()
	assert.Zero(t, used)
}

// A stream gives back what it set aside and did not use, and many parallel streams of one
// peer cannot take more than its share.
func TestFreeBudgetRefundsAndParallelStreams(t *testing.T) {
	l := DefaultLimits()
	l.FreePeerDaily = 4 << 20
	svc, _ := budgetExit(t, l)

	m := newMeter(svc, testClient)
	require.True(t, m.spend(100))
	m.close()
	assert.Equal(t, int64(4<<20-100), svc.allowance(testClient), "only the 100 bytes moved are counted")

	var wg sync.WaitGroup
	var mu sync.Mutex
	var spent int64
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := spendAll(svc, testClient, 16<<10)
			mu.Lock()
			spent += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, spent, int64(4<<20-100), "parallel streams never exceed the share")
	assert.Zero(t, svc.allowance(testClient))
	used, _ := svc.FreeMonth()
	assert.Equal(t, int64(4<<20), used, "the share is used up exactly: the chunk that crossed it is dropped, not refunded")
	assert.Greater(t, spent, int64(4<<20-100-16*(16<<10)), "each stream loses at most the chunk it was cut on")
}

// The counter tables stay bounded however many identities show up.
func TestFreeBudgetTablesBounded(t *testing.T) {
	l := DefaultLimits()
	l.FreePeerDaily = 1 << 20
	svc, _ := budgetExit(t, l)
	for i := 0; i < quotaPeers+100; i++ {
		var id types.PeerID
		id[0], id[1], id[2] = byte(i), byte(i>>8), byte(i>>16)
		svc.takeFree(id, "", int64(1+i%7), l)
	}
	svc.budget.mu.Lock()
	defer svc.budget.mu.Unlock()
	assert.LessOrEqual(t, len(svc.budget.peers), quotaPeers)
}
