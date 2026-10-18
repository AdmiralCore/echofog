package exit_gateway

import (
	"sync"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Streams reserve allowance in slices, yet many of them at once still cannot spend more than
// the peer has, and what they set aside and did not use comes back when they end.
func TestMetersShareOneAllowanceAndGiveBackTheRest(t *testing.T) {
	svc, l := paidExit(t, 1000)
	tag := PayTag(testClient, testExit)
	bought, ok := l.Credit(tag, "tx", MinPayMicro, price49)
	require.True(t, ok)
	total := 1000 + bought

	var wg sync.WaitGroup
	var mu sync.Mutex
	var spent int64
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := newMeter(svc, testClient)
			defer m.close()
			for j := 0; j < 200; j++ {
				if !m.spend(32 << 10) {
					return
				}
				mu.Lock()
				spent += 32 << 10
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, spent, total+OverdraftBytes, "parallel streams never spend more than the peer has")
	credit, _ := l.Balance(tag)
	assert.Equal(t, spent, svc.quotas[testClient].used+(bought-credit),
		"what was taken from the pools is exactly what was spent: the unused slices went back")
}

// A short stream reserves a whole slice but pays only for what it moved.
func TestMeterRefundsWhatAStreamDidNotUse(t *testing.T) {
	svc, l := paidExit(t, 0)
	tag := PayTag(testClient, testExit)
	bought, _ := l.Credit(tag, "tx", MinPayMicro, price49)

	m := newMeter(svc, testClient)
	require.True(t, m.spend(1500))
	credit, _ := l.Balance(tag)
	assert.Equal(t, bought-meterSlice, credit, "a slice is set aside while the stream runs")
	m.close()
	credit, _ = l.Balance(tag)
	assert.Equal(t, bought-1500, credit)

	m = newMeter(svc, testClient)
	require.True(t, m.spend(700))
	m.close()
	assert.Equal(t, int64(0), svc.quotas[testClient].used)
	credit, _ = l.Balance(tag)
	assert.Equal(t, bought-2200, credit)
}

// The free quota is taken before paid credit, and handed back the same way.
func TestMeterSpendsFreeQuotaFirst(t *testing.T) {
	svc, l := paidExit(t, 1000)
	tag := PayTag(testClient, testExit)
	bought, _ := l.Credit(tag, "tx", MinPayMicro, price49)

	m := newMeter(svc, testClient)
	require.True(t, m.spend(600))
	m.close()
	credit, _ := l.Balance(tag)
	assert.Equal(t, bought, credit, "paid credit untouched while free quota lasts")
	assert.Equal(t, bought+400+OverdraftBytes, svc.allowance(testClient), "400 free bytes left, credit and overdraft whole")
}

// A peer put on the free list in the middle of a stream rides free from the next slice on, and
// one taken off it pays again.
func TestMeterFollowsTheFreeList(t *testing.T) {
	svc, _ := paidExit(t, 100)
	friend := types.PeerID{7}
	svc.SetFreePeers([]types.PeerID{friend})
	m := newMeter(svc, friend)
	for i := 0; i < 8; i++ {
		require.True(t, m.spend(meterSlice))
	}
	svc.SetFreePeers(nil)
	require.False(t, m.spend(meterSlice), "the free ride ends once the slice it had is used")
	m.close()
}

// The old per-read path, for comparison:
//
//	go test ./internal/network/exit_gateway -run XXX -bench Meter -benchmem
func BenchmarkConsumePerRead(b *testing.B) {
	svc, l := paidExit(&testing.T{}, 0)
	l.Credit(PayTag(testClient, testExit), "tx", 1<<40, 1)
	b.SetBytes(32 << 10)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			svc.consume(testClient, 32<<10)
		}
	})
}

func BenchmarkMeterSpend(b *testing.B) {
	svc, l := paidExit(&testing.T{}, 0)
	l.Credit(PayTag(testClient, testExit), "tx", 1<<40, 1)
	b.SetBytes(32 << 10)
	b.RunParallel(func(pb *testing.PB) {
		m := newMeter(svc, testClient)
		defer m.close()
		for pb.Next() {
			m.spend(32 << 10)
		}
	})
}
