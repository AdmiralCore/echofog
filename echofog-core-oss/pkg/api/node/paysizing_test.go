package node

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/stretchr/testify/assert"
)

func TestVoucherSizingFollowsTheSpeed(t *testing.T) {
	chunk, floor := voucherSizing(0)
	assert.Equal(t, int64(voucherChunkMB), chunk)
	assert.Equal(t, int64(16<<20), floor, "idle: the old 64 MB chunk bought at 16 MB")

	chunk, floor = voucherSizing(4 << 20) // 4 MB/s, a 4K stream
	assert.GreaterOrEqual(t, chunk, int64(240), "a minute of traffic per voucher")
	assert.Equal(t, int64(80<<20), floor, "bought while 20 s are left")

	chunk, _ = voucherSizing(1 << 30)
	assert.Equal(t, int64(voucherMaxChunkMB), chunk, "capped")
}

// At 4 MB/s, 40 MB left is enough for the fixed 16 MB floor, but not with 20 s of headroom.
func TestFastTrafficBuysTheNextVoucherEarly(t *testing.T) {
	info := exit_gateway.AccountInfo{Enabled: true, PricePerMB: 49, Vault: &exit_gateway.VaultTerms{NanoPerMB: 10, Node: "0:ee", Credit: 40 << 20}}
	set := autoPaySettings{Enabled: true}
	set.defaults()
	now := time.Unix(1_800_000_000, 0)
	assert.Equal(t, payNothing, decideVoucherAt(info, set, 0, now, time.Time{}, 0).Action)
	d := decideVoucherAt(info, set, 0, now, time.Time{}, 4<<20)
	assert.Equal(t, payNow, d.Action)
	assert.Greater(t, d.Micro, int64(49*voucherChunkMB), "a bigger voucher for faster traffic")
}

func TestTrafficRateRisesAtOnceAndFallsSlowly(t *testing.T) {
	var p payState
	t0 := time.Unix(1_800_000_000, 0)
	p.noteTraffic(0, t0)
	p.noteTraffic(10<<20, t0.Add(2*time.Second))
	assert.InDelta(t, 5<<20, p.rate, 1, "a burst counts in full")
	p.noteTraffic(10<<20, t0.Add(4*time.Second))
	assert.InDelta(t, 3<<20, p.rate, 1, "and fades over a few rounds")
}
