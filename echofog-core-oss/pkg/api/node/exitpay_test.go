package node

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const price = 49 // micro-USDT per MB, about 5 cents a GB

func accountWith(remainingGB float64) exit_gateway.AccountInfo {
	return exit_gateway.AccountInfo{
		Version: 1, Enabled: true, PricePerMB: price, PayAddress: "UQ-exit", Tag: "ef1:0123456789abcdef",
		MinPayMicro: exit_gateway.MinPayMicro, Credit: int64(remainingGB * (1 << 30)),
	}
}

func settings(enabled bool) autoPaySettings {
	s := autoPaySettings{Enabled: enabled, ChunkMicro: microPerUSDT, DailyCapMicro: 5 * microPerUSDT}
	s.defaults()
	return s
}

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func TestNothingIsPaidWhileThereIsEnoughLeft(t *testing.T) {
	// one 1 USDT chunk is ~20 GB here, so a top-up is due below ~5 GB
	d := decidePayment(accountWith(8), settings(true), 0, t0, time.Time{}, time.Time{})
	assert.Equal(t, payNothing, d.Action)
}

func TestLowBalanceTriggersTheConfiguredChunk(t *testing.T) {
	d := decidePayment(accountWith(2), settings(true), 0, t0, time.Time{}, time.Time{})
	assert.Equal(t, payNow, d.Action)
	assert.Equal(t, int64(microPerUSDT), d.Micro)
}

func TestWithoutConsentTheUserIsAskedNotCharged(t *testing.T) {
	d := decidePayment(accountWith(0.1), settings(false), 0, t0, time.Time{}, time.Time{})
	assert.Equal(t, payAsk, d.Action, "automatic payments are off: show the prompt")
	assert.Equal(t, int64(microPerUSDT), d.Micro)
}

func TestFreeAndUnpayableExitsAreLeftAlone(t *testing.T) {
	free := accountWith(0)
	free.PricePerMB = 0
	assert.Equal(t, payNothing, decidePayment(free, settings(true), 0, t0, time.Time{}, time.Time{}).Action)

	noAddr := accountWith(0)
	noAddr.PayAddress = ""
	assert.Equal(t, payNothing, decidePayment(noAddr, settings(true), 0, t0, time.Time{}, time.Time{}).Action)

	off := accountWith(0)
	off.Enabled = false
	assert.Equal(t, payNothing, decidePayment(off, settings(true), 0, t0, time.Time{}, time.Time{}).Action)
}

func TestDailyCapStopsAutomaticPayments(t *testing.T) {
	d := decidePayment(accountWith(0), settings(true), 5*microPerUSDT, t0, time.Time{}, time.Time{})
	assert.Equal(t, payNothing, d.Action)
	assert.Contains(t, d.Reason, "daily limit")

	d = decidePayment(accountWith(0), settings(true), 4*microPerUSDT, t0, time.Time{}, time.Time{})
	assert.Equal(t, payNow, d.Action, "still room for one more chunk")
}

func TestOnePaymentAtATime(t *testing.T) {
	waiting := decidePayment(accountWith(0), settings(true), 0, t0, t0.Add(time.Minute), time.Time{})
	assert.Equal(t, payNothing, waiting.Action, "the previous transfer is still confirming")

	blocked := decidePayment(accountWith(0), settings(true), 0, t0, time.Time{}, t0.Add(time.Minute))
	assert.Equal(t, payNothing, blocked.Action)
	assert.NotEmpty(t, blocked.Reason)
}

func TestThresholdKeepsRoomForConfirmationTime(t *testing.T) {
	info := accountWith(0)
	// a tiny top-up still keeps 256 MB in hand to bridge the on-chain delay
	assert.Equal(t, int64(topUpFloorBytes), topUpThreshold(info, 20_000)) // 0.02 USDT ~ 408 MB
	// but never asks for more than the chunk itself buys
	assert.Equal(t, info.BytesFor(10_000), topUpThreshold(info, 10_000)) // ~204 MB < floor
}

func TestMinimumTransferIsRespected(t *testing.T) {
	s := settings(true)
	s.ChunkMicro = 1000 // below what the exit credits
	d := decidePayment(accountWith(0), s, 0, t0, time.Time{}, time.Time{})
	require.Equal(t, payNow, d.Action)
	assert.Equal(t, int64(exit_gateway.MinPayMicro), d.Micro)
}

// --- the guard round, with a fake exit and wallet ---

type fakeExit struct {
	mu      sync.Mutex
	info    exit_gateway.AccountInfo
	queryEr error
	payEr   error
	paid    []int64
}

func (f *fakeExit) env() payEnv {
	return payEnv{
		query: func(context.Context) (*exit_gateway.AccountInfo, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.queryEr != nil {
				return nil, f.queryEr
			}
			c := f.info
			return &c, nil
		},
		pay: func(_ context.Context, micro int64, _ *exit_gateway.AccountInfo) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.payEr != nil {
				return f.payEr
			}
			f.paid = append(f.paid, micro)
			return nil
		},
	}
}

func (f *fakeExit) credit(bytes int64) { f.mu.Lock(); f.info.Credit += bytes; f.mu.Unlock() }

func newGuard(enabled bool, now *time.Time) *payState {
	p := &payState{now: func() time.Time { return *now }}
	p.set = settings(enabled)
	p.loaded = true
	return p
}

func TestGuardPaysOnceThenWaitsForTheCredit(t *testing.T) {
	now := t0
	ex := &fakeExit{info: accountWith(1)}
	p := newGuard(true, &now)
	ctx := context.Background()

	wait := p.step(ctx, ex.env())
	assert.Equal(t, []int64{microPerUSDT}, ex.paid)
	assert.Equal(t, payGuardPending, wait, "watch closely while a payment is in flight")
	assert.True(t, p.status.Pending)

	// the balance is still low and the transfer has not shown up: do not pay again
	now = now.Add(30 * time.Second)
	p.step(ctx, ex.env())
	assert.Len(t, ex.paid, 1)

	// the exit credits it: back to normal, nothing pending
	ex.credit(int64(20 << 30))
	now = now.Add(10 * time.Second)
	p.step(ctx, ex.env())
	assert.False(t, p.status.Pending)
	assert.Empty(t, p.status.LastError)
	assert.Len(t, ex.paid, 1)
	assert.Equal(t, "1", p.status.SpentTodayUSDT)
}

func TestGuardStopsAfterAPaymentThatNeverArrives(t *testing.T) {
	now := t0
	ex := &fakeExit{info: accountWith(0)}
	p := newGuard(true, &now)
	ctx := context.Background()

	p.step(ctx, ex.env())
	require.Len(t, ex.paid, 1)

	now = now.Add(payPendingFor + time.Second) // the deadline passes with no credit
	p.step(ctx, ex.env())
	assert.Contains(t, p.status.LastError, "not credited")
	assert.Len(t, ex.paid, 1, "no second payment into a black hole")

	now = now.Add(time.Minute)
	p.step(ctx, ex.env())
	assert.Len(t, ex.paid, 1, "still paused")

	now = now.Add(payBlockedFor)
	p.step(ctx, ex.env())
	assert.Len(t, ex.paid, 2, "automatic payments resume after the pause")
}

func TestGuardBacksOffWhenThePaymentFails(t *testing.T) {
	now := t0
	ex := &fakeExit{info: accountWith(0), payEr: errors.New("not enough USDT on the balance")}
	p := newGuard(true, &now)
	ctx := context.Background()

	p.step(ctx, ex.env())
	assert.Contains(t, p.status.LastError, "not enough USDT")
	assert.Empty(t, ex.paid)
	assert.Equal(t, "0", p.status.SpentTodayUSDT, "a failed attempt costs nothing and is not counted")

	ex.payEr = nil
	now = now.Add(10 * time.Second)
	p.step(ctx, ex.env())
	assert.Empty(t, ex.paid, "wait before retrying")

	now = now.Add(payRetryAfter)
	p.step(ctx, ex.env())
	assert.Len(t, ex.paid, 1)
	assert.Equal(t, "1", p.status.SpentTodayUSDT)
}

func TestGuardNeverPaysWithoutConsent(t *testing.T) {
	now := t0
	ex := &fakeExit{info: accountWith(0)}
	p := newGuard(false, &now)
	p.step(context.Background(), ex.env())
	assert.Empty(t, ex.paid)
	assert.True(t, p.status.NeedsPayment, "the interface asks instead")
}

func TestGuardRespectsTheDailyCapAndResetsNextDay(t *testing.T) {
	now := t0
	ex := &fakeExit{info: accountWith(0)}
	p := newGuard(true, &now)
	p.set.DailyCapMicro = 2 * microPerUSDT
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		ex.mu.Lock()
		ex.info.Credit = 0
		ex.mu.Unlock()
		now = now.Add(payPendingFor + payBlockedFor + time.Minute) // clear any pause between tries
		p.pendingUntil, p.blockedUntil = time.Time{}, time.Time{}
		p.step(ctx, ex.env())
	}
	assert.Len(t, ex.paid, 2, "stops at the 2 USDT daily cap")
	assert.Contains(t, p.status.Note, "daily limit")

	now = now.Add(24 * time.Hour)
	p.pendingUntil, p.blockedUntil = time.Time{}, time.Time{}
	p.step(ctx, ex.env())
	assert.Len(t, ex.paid, 3, "a new day, a new allowance")
}

func TestGuardReportsAnUnreachableExit(t *testing.T) {
	now := t0
	ex := &fakeExit{queryEr: errors.New("timeout")}
	p := newGuard(true, &now)
	wait := p.step(context.Background(), ex.env())
	assert.Equal(t, payGuardEvery, wait)
	assert.False(t, p.status.Active)
	assert.Contains(t, p.status.LastError, "cannot reach the exit")
}

func TestAutoPaySettingsPersistAndAreValidated(t *testing.T) {
	n := &NodeApi{}
	n.pay.path = t.TempDir() + "/autopay.json"
	require.NoError(t, n.SetAutoPay(true, "2", "10"))

	again := &payState{path: n.pay.path}
	again.loadLocked()
	assert.True(t, again.set.Enabled)
	assert.Equal(t, int64(2*microPerUSDT), again.set.ChunkMicro)
	assert.Equal(t, int64(10*microPerUSDT), again.set.DailyCapMicro)

	assert.Error(t, n.SetAutoPay(true, "abc", "10"))
	assert.Error(t, n.SetAutoPay(true, "0.001", "10"), "below what an exit credits")
	assert.Error(t, n.SetAutoPay(true, "500", "1000"), "beyond the sane maximum")
	assert.Error(t, n.SetAutoPay(true, "5", "2"), "the daily limit must cover one top-up")
}

func TestOnlyFinishedIncomingUSDTWithACommentCanPay(t *testing.T) {
	now := time.Now()
	tx := func(mod func(*tonwallet.Tx)) tonwallet.Tx {
		v := tonwallet.Tx{Asset: "USDT", Hash: "h", Time: now, Incoming: true, AmountNano: "1000000", Comment: "ef1:0123456789abcdef"}
		mod(&v)
		return v
	}
	got := paymentsFromTxs([]tonwallet.Tx{
		tx(func(*tonwallet.Tx) {}), // valid
		tx(func(v *tonwallet.Tx) { v.Incoming = false }),
		tx(func(v *tonwallet.Tx) { v.Failed = true }),
		tx(func(v *tonwallet.Tx) { v.Asset = "TON" }),
		tx(func(v *tonwallet.Tx) { v.Asset = "SCAM" }),
		tx(func(v *tonwallet.Tx) { v.Comment = "" }),
		tx(func(v *tonwallet.Tx) { v.AmountNano = "-5" }),
		tx(func(v *tonwallet.Tx) { v.AmountNano = "99999999999999999999999999" }),
		tx(func(v *tonwallet.Tx) { v.AmountNano = "junk" }),
	}, 0) // no TON rate: TON waits

	require.Len(t, got, 1)
	assert.Equal(t, int64(1_000_000), got[0].Micro)
	assert.Equal(t, "ef1:0123456789abcdef", got[0].Comment)
}

// TON is credited at the rate when it arrives; without a rate it waits for the next round.
func TestTONPaymentsAreValuedAtTheRate(t *testing.T) {
	now := time.Now()
	ton := tonwallet.Tx{Asset: "TON", Hash: "t", Time: now, Incoming: true, AmountNano: "2000000000", Comment: "ef1:0123456789abcdef"}
	got := paymentsFromTxs([]tonwallet.Tx{ton}, 5.0)
	require.Len(t, got, 1)
	assert.Equal(t, int64(10_000_000), got[0].Micro, "2 TON at $5 is $10")
	assert.Equal(t, "TON", got[0].Asset)
	assert.Empty(t, paymentsFromTxs([]tonwallet.Tx{ton}, 0))
}

func TestTONAmountCoversThePriceWithAMargin(t *testing.T) {
	nano := microUSDToTonNano(1_000_000, 5.0, tonPayMargin) // $1 at $5 per TON, plus 2%
	assert.Equal(t, "0.204", tonwallet.FormatTON(nano)[:5])
	back := tonNanoToMicroUSD(nano, 5.0)
	assert.GreaterOrEqual(t, back, int64(1_000_000))
	// the exit's rate may be up to the margin lower and the payment still covers the price
	assert.GreaterOrEqual(t, tonNanoToMicroUSD(nano, 5.0/1.02), int64(999_999))
}

func TestChoosePayAsset(t *testing.T) {
	both := exit_gateway.AccountInfo{Assets: exit_gateway.PayAssets}
	usdtOnly := exit_gateway.AccountInfo{} // an exit of an older version
	ton := func(s string) *big.Int { n, _ := tonwallet.ParseTON(s); return n }
	usdt := func(micro int64) *big.Int { return big.NewInt(micro) }
	const dollar = 1_000_000

	c, err := choosePayAsset(PayAuto, both, dollar, usdt(5*dollar), ton("1"), 5)
	require.NoError(t, err)
	assert.Equal(t, PayUSDT, c.Asset, "USDT when there is enough of it and TON for the fee")

	c, err = choosePayAsset(PayAuto, both, dollar, usdt(0), ton("1"), 5)
	require.NoError(t, err)
	assert.Equal(t, PayTON, c.Asset, "TON when there is no USDT")
	assert.True(t, c.TonNano.Cmp(ton("0.2")) > 0)

	c, err = choosePayAsset(PayAuto, both, dollar, usdt(5*dollar), ton("0.01"), 5)
	assert.Error(t, err, "USDT without TON for its fee, and too little TON")

	c, err = choosePayAsset(PayTON, both, dollar, usdt(5*dollar), ton("1"), 5)
	require.NoError(t, err)
	assert.Equal(t, PayTON, c.Asset, "the user's choice wins")

	_, err = choosePayAsset(PayTON, usdtOnly, dollar, usdt(0), ton("10"), 5)
	assert.ErrorContains(t, err, "USDT only")

	c, err = choosePayAsset(PayAuto, usdtOnly, dollar, usdt(2*dollar), ton("1"), 5)
	require.NoError(t, err)
	assert.Equal(t, PayUSDT, c.Asset)

	_, err = choosePayAsset(PayTON, both, dollar, usdt(0), ton("10"), 0)
	assert.ErrorContains(t, err, "price is not known")

	_, err = choosePayAsset(PayAuto, both, dollar, usdt(0), ton("0.1"), 5)
	assert.ErrorContains(t, err, "TON needed")
}
