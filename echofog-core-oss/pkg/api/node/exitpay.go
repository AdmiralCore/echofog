package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
)

// The client side of paying an exit. The exit counts bytes; the client watches how many are left
// and buys the next chunk before they run out. Nothing is ever paid without the user's consent:
// either an explicit "pay" press, or automatic top-ups they switched on, within a daily cap.

const (
	microPerUSDT = 1_000_000

	defaultChunkMicro    = 1 * microPerUSDT
	defaultDailyCapMicro = 5 * microPerUSDT
	maxManualPayMicro    = 200 * microPerUSDT // typo guard for a hand-typed amount

	// topUpFloorBytes: always keep this much in hand, so a payment still confirming on-chain
	// (10-30 s) does not cut a running call or stream.
	topUpFloorBytes = 256 << 20

	payGuardEvery   = 10 * time.Second
	payGuardPending = 3 * time.Second
	payPendingFor   = 3 * time.Minute  // how long a transfer may take to be credited
	payBlockedFor   = 10 * time.Minute // no new automatic payment after one that never arrived
	payRetryAfter   = 60 * time.Second // after a failed attempt (no funds, network down)
)

type autoPaySettings struct {
	Enabled       bool             `json:"enabled"`
	ChunkMicro    int64            `json:"chunk_micro"`
	DailyCapMicro int64            `json:"daily_cap_micro"`
	Spent         map[string]int64 `json:"spent"` // UTC day -> micro-USD paid to exits
	Asset         string           `json:"asset"` // "auto", "TON" or "USDT"
}

// Pay assets the user can choose. Prices are in US dollars either way; TON is converted at the
// current rate, with tonPayMargin on top for the difference between our rate and the exit's.
const (
	PayAuto      = "auto"
	PayTON       = "TON"
	PayUSDT      = "USDT"
	tonPayMargin = 0.02
)

func (s *autoPaySettings) defaults() {
	if s.ChunkMicro <= 0 {
		s.ChunkMicro = defaultChunkMicro
	}
	if s.DailyCapMicro <= 0 {
		s.DailyCapMicro = defaultDailyCapMicro
	}
	if s.Spent == nil {
		s.Spent = map[string]int64{}
	}
	if s.Asset != PayTON && s.Asset != PayUSDT {
		s.Asset = PayAuto
	}
}

// payChoice is how one payment is made: the asset and, for TON, the amount in nano-TON.
type payChoice struct {
	Asset   string
	TonNano *big.Int
}

// choosePayAsset picks how to pay micro-USD to an exit, from what it takes and what the wallet
// holds. Automatic: USDT when there is enough of it (and TON for its fee), otherwise TON at rate.
func choosePayAsset(pref string, info exit_gateway.AccountInfo, micro int64, usdtMicro, tonNano *big.Int, rate float64) (payChoice, error) {
	if usdtMicro == nil {
		usdtMicro = new(big.Int)
	}
	if tonNano == nil {
		tonNano = new(big.Int)
	}
	usdtFee := new(big.Int).Add(tonwallet.JettonGas, tonwallet.FeeReserve)
	usdtOK := info.Accepts(PayUSDT) && usdtMicro.Cmp(big.NewInt(micro)) >= 0 && tonNano.Cmp(usdtFee) >= 0
	var needTON *big.Int
	tonOK := false
	if rate > 0 && info.Accepts(PayTON) {
		needTON = microUSDToTonNano(micro, rate, tonPayMargin)
		tonOK = tonNano.Cmp(new(big.Int).Add(needTON, tonwallet.FeeReserve)) >= 0
	}
	usd := tonwallet.FormatUnits(big.NewInt(micro), 6)
	switch {
	case pref == PayUSDT && usdtOK, pref != PayTON && pref != PayUSDT && usdtOK:
		return payChoice{Asset: PayUSDT}, nil
	case (pref == PayTON || pref != PayUSDT) && tonOK:
		return payChoice{Asset: PayTON, TonNano: needTON}, nil
	}
	switch {
	case pref == PayUSDT && !info.Accepts(PayUSDT):
		return payChoice{}, errors.New("this exit does not take USDT")
	case pref == PayTON && !info.Accepts(PayTON):
		return payChoice{}, errors.New("this exit takes USDT only")
	case pref == PayTON && rate <= 0:
		return payChoice{}, errors.New("the TON price is not known right now, try again in a minute")
	case pref == PayUSDT:
		return payChoice{}, fmt.Errorf("not enough USDT: %s USDT needed, plus about 0.065 TON for the network fee", usd)
	case pref == PayTON:
		return payChoice{}, fmt.Errorf("not enough TON: about %s TON needed", tonwallet.FormatTON(new(big.Int).Add(needTON, tonwallet.FeeReserve)))
	}
	if needTON != nil {
		return payChoice{}, fmt.Errorf("not enough funds: %s USDT (plus 0.065 TON for the fee) or about %s TON needed", usd, tonwallet.FormatTON(new(big.Int).Add(needTON, tonwallet.FeeReserve)))
	}
	return payChoice{}, fmt.Errorf("not enough USDT: %s USDT needed, plus about 0.065 TON for the network fee", usd)
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// payAction is what the guard concluded for this round.
type payAction int

const (
	payNothing payAction = iota // nothing to do (enough left, free exit, or already waiting)
	payAsk                      // low, and the user has not allowed automatic payments
	payNow                      // low, allowed: pay the chunk
)

type payDecision struct {
	Action payAction
	Micro  int64
	Reason string // why nothing is paid although the balance is low (shown to the user)
}

// topUpThreshold is the balance below which the next chunk is bought: a quarter of a chunk, but
// never less than topUpFloorBytes (and never more than the chunk itself).
func topUpThreshold(info exit_gateway.AccountInfo, chunkMicro int64) int64 {
	chunkBytes := info.BytesFor(chunkMicro)
	th := chunkBytes / 4
	if th < topUpFloorBytes {
		th = topUpFloorBytes
	}
	if th > chunkBytes {
		th = chunkBytes
	}
	return th
}

// decidePayment is the whole policy in one pure function.
func decidePayment(info exit_gateway.AccountInfo, set autoPaySettings, spentToday int64, now, pendingUntil, blockedUntil time.Time) payDecision {
	if !info.CanPay() {
		return payDecision{}
	}
	chunk := set.ChunkMicro
	if chunk < info.MinPayMicro {
		chunk = info.MinPayMicro
	}
	if info.Remaining() >= topUpThreshold(info, chunk) {
		return payDecision{}
	}
	if !set.Enabled {
		return payDecision{Action: payAsk, Micro: chunk}
	}
	if now.Before(pendingUntil) {
		return payDecision{} // a payment is on its way
	}
	if now.Before(blockedUntil) {
		return payDecision{Reason: "the last payment has not been credited yet, automatic payments are paused"}
	}
	if spentToday+chunk > set.DailyCapMicro {
		return payDecision{Reason: "the daily limit for automatic payments is reached"}
	}
	return payDecision{Action: payNow, Micro: chunk}
}

// ExitPaymentStatus is what the interface shows about paying the current exit.
type ExitPaymentStatus struct {
	Active         bool    `json:"active"`         // proxy is on and the exit answered
	CanPay         bool    `json:"canPay"`         // false: free exit, or one that takes no payments
	PricePerGB     string  `json:"pricePerGb"`     // USDT, e.g. "0.05"
	RemainingGB    float64 `json:"remainingGb"`    // free + paid left (can dip below 0 in the overdraft)
	NeedsPayment   bool    `json:"needsPayment"`   // low and automatic payments are off: ask the user
	AutoPay        bool    `json:"autoPay"`        // automatic top-ups enabled
	ChunkUSDT      string  `json:"chunkUsdt"`      // size of one top-up
	ChunkGB        float64 `json:"chunkGb"`        // what one top-up buys here
	DailyCapUSDT   string  `json:"dailyCapUsdt"`   // automatic payments stop at this per day
	PayAsset       string  `json:"payAsset"`       // "auto", "TON" or "USDT"
	TakesTON       bool    `json:"takesTon"`       // the exit takes TON as well
	ChunkTON       string  `json:"chunkTon"`       // what a top-up costs in TON now, if the rate is known
	SpentTodayUSDT string  `json:"spentTodayUsdt"` // paid to exits today
	Pending        bool    `json:"pending"`        // a payment was sent and is not credited yet
	Note           string  `json:"note"`           // why payments are paused, if they are
	LastError      string  `json:"lastError"`
	PaysByVault    bool    `json:"paysByVault"` // paid with vouchers from the TON vault
}

// payState is the client's payment bookkeeping for the exit it is connected to.
type payState struct {
	mu           sync.Mutex
	path         string
	set          autoPaySettings
	loaded       bool
	cancel       context.CancelFunc
	status       ExitPaymentStatus
	pendingUntil time.Time
	creditBefore int64
	blockedUntil time.Time
	lastErr      string
	now          func() time.Time
	// the proxy's traffic rate in bytes/s, measured between guard rounds
	rate      float64
	rateBytes uint64
	rateAt    time.Time
}

func (p *payState) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *payState) loadLocked() {
	if p.loaded {
		return
	}
	p.loaded = true
	if p.path != "" {
		if raw, err := os.ReadFile(p.path); err == nil {
			_ = json.Unmarshal(raw, &p.set)
		}
	}
	p.set.defaults()
}

func (p *payState) saveLocked() {
	if p.path == "" {
		return
	}
	// drop old days so the file stays tiny
	cut := day(p.clock().AddDate(0, 0, -30))
	for d := range p.set.Spent {
		if d < cut {
			delete(p.set.Spent, d)
		}
	}
	raw, err := json.Marshal(&p.set)
	if err != nil || os.MkdirAll(filepath.Dir(p.path), 0o700) != nil {
		return
	}
	tmp := p.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, p.path)
	}
}

// payEnv is what one guard round needs from the outside, so a round can be tested without a
// network or a wallet.
type payEnv struct {
	query func(ctx context.Context) (*exit_gateway.AccountInfo, error)
	pay   func(ctx context.Context, micro int64, info *exit_gateway.AccountInfo) error
	// vault tells whether this round pays from the TON vault (nil: never); payVoucher does it.
	vault      func(ctx context.Context, info *exit_gateway.AccountInfo) bool
	payVoucher func(ctx context.Context, micro int64, info *exit_gateway.AccountInfo) error
	// traffic is the proxy's total bytes so far (nil: unknown); it sizes vouchers to the speed
	traffic func() uint64
}

// voucherSizing is how much to buy per voucher and how early, for traffic running at rate
// bytes/s: enough for a minute, bought while 20 s are left. A 4K stream at 4 MB/s would empty a
// fixed 16 MB floor in four seconds, which is about what a voucher round trip takes.
func voucherSizing(rate float64) (chunkMB, floor int64) {
	chunkMB, floor = voucherChunkMB, voucherChunkMB<<20/4
	if rate <= 0 {
		return
	}
	if mb := int64(rate*60)>>20 + 1; mb > chunkMB {
		chunkMB = min(mb, voucherMaxChunkMB)
	}
	floor = max(floor, int64(rate*20))
	return
}

// noteTraffic updates the traffic rate (an average that follows changes within ~10 s).
func (p *payState) noteTraffic(total uint64, now time.Time) {
	if !p.rateAt.IsZero() && total >= p.rateBytes {
		if dt := now.Sub(p.rateAt).Seconds(); dt > 0.2 {
			cur := float64(total-p.rateBytes) / dt
			p.rate = 0.6*p.rate + 0.4*cur
			if cur > p.rate {
				p.rate = cur // speeding up: follow at once, so the next voucher is big enough
			}
		}
	}
	p.rateBytes, p.rateAt = total, now
}

// decideVoucher is decidePayment for an exit paid with vouchers from the vault. Nothing goes
// on-chain per chunk, so the chunk is small (64 MB) and the next one is bought when 16 MB are
// left. A voucher is credited at once, so there is never a payment "on its way".
func decideVoucher(info exit_gateway.AccountInfo, set autoPaySettings, spentToday int64, now, blockedUntil time.Time) payDecision {
	return decideVoucherAt(info, set, spentToday, now, blockedUntil, 0)
}

// decideVoucherAt is decideVoucher for traffic running at rate bytes/s.
func decideVoucherAt(info exit_gateway.AccountInfo, set autoPaySettings, spentToday int64, now, blockedUntil time.Time, rate float64) payDecision {
	if !info.TakesVouchers() {
		return payDecision{}
	}
	chunkMB, floor := voucherSizing(rate)
	chunk := max(1, info.PricePerMB*chunkMB)
	if info.Remaining() >= floor {
		return payDecision{}
	}
	if !set.Enabled {
		return payDecision{Action: payAsk, Micro: chunk}
	}
	if now.Before(blockedUntil) {
		return payDecision{Reason: "the last voucher was refused, trying again shortly"}
	}
	if spentToday+chunk > set.DailyCapMicro {
		return payDecision{Reason: "the daily limit for automatic payments is reached"}
	}
	return payDecision{Action: payNow, Micro: chunk}
}

// step runs one guard round: read the account, notice a credited or missing payment, and pay the
// next chunk if the policy says so. It returns how long to wait before the next round.
func (p *payState) step(ctx context.Context, env payEnv) time.Duration {
	info, err := env.query(ctx)
	useVault := err == nil && env.vault != nil && env.payVoucher != nil && env.vault(ctx, info)
	p.mu.Lock()
	p.loadLocked()
	now := p.clock()
	if env.traffic != nil {
		p.noteTraffic(env.traffic(), now)
	}
	if err != nil {
		p.status.Active = false
		p.lastErr = "cannot reach the exit: " + err.Error()
		p.status.LastError = p.lastErr
		p.mu.Unlock()
		return payGuardEvery
	}

	// a payment sent earlier: credited, or overdue?
	if !p.pendingUntil.IsZero() {
		switch {
		case info.Credit > p.creditBefore:
			p.pendingUntil, p.blockedUntil, p.lastErr = time.Time{}, time.Time{}, ""
		case now.After(p.pendingUntil):
			p.pendingUntil = time.Time{}
			p.blockedUntil = now.Add(payBlockedFor)
			p.lastErr = "the payment was sent but the exit has not credited it; check the transaction in the wallet history"
		}
	}

	spent := p.set.Spent[day(now)]
	dec := decidePayment(*info, p.set, spent, now, p.pendingUntil, p.blockedUntil)
	if useVault {
		dec = decideVoucherAt(*info, p.set, spent, now, p.blockedUntil, p.rate)
	}
	p.publishLocked(*info, dec, spent, now)
	p.status.PaysByVault = useVault

	if dec.Action != payNow {
		wait := payGuardEvery
		switch {
		case useVault:
			wait = voucherGuardEvery
		case !p.pendingUntil.IsZero():
			wait = payGuardPending
		}
		p.mu.Unlock()
		return wait
	}
	p.mu.Unlock()

	pay := env.pay
	if useVault {
		pay = env.payVoucher
	}
	payErr := pay(ctx, dec.Micro, info)

	p.mu.Lock()
	defer p.mu.Unlock()
	now = p.clock()
	if payErr != nil {
		p.lastErr = "automatic payment failed: " + payErr.Error()
		p.status.LastError = p.lastErr
		p.blockedUntil = now.Add(payRetryAfter)
		return payGuardEvery
	}
	if useVault {
		// credited by the exit before it answered: nothing to wait for
		p.set.Spent[day(now)] += dec.Micro
		p.lastErr = ""
		p.saveLocked()
		p.publishLocked(*info, payDecision{}, p.set.Spent[day(now)], now)
		p.status.PaysByVault = true
		return voucherGuardEvery
	}
	p.set.Spent[day(now)] += dec.Micro
	p.pendingUntil = now.Add(payPendingFor)
	p.creditBefore = info.Credit
	p.lastErr = ""
	p.saveLocked()
	p.publishLocked(*info, payDecision{}, p.set.Spent[day(now)], now)
	return payGuardPending
}

func (p *payState) publishLocked(info exit_gateway.AccountInfo, dec payDecision, spent int64, now time.Time) {
	chunk := p.set.ChunkMicro
	if chunk < info.MinPayMicro {
		chunk = info.MinPayMicro
	}
	st := ExitPaymentStatus{
		Active:         true,
		CanPay:         info.CanPay(),
		RemainingGB:    float64(info.Remaining()) / float64(1<<30),
		NeedsPayment:   dec.Action == payAsk,
		AutoPay:        p.set.Enabled,
		ChunkUSDT:      tonwallet.FormatUnits(big.NewInt(chunk), 6),
		DailyCapUSDT:   tonwallet.FormatUnits(big.NewInt(p.set.DailyCapMicro), 6),
		SpentTodayUSDT: tonwallet.FormatUnits(big.NewInt(spent), 6),
		PayAsset:       p.set.Asset,
		TakesTON:       info.Accepts(PayTON),
		Pending:        now.Before(p.pendingUntil),
		Note:           dec.Reason,
		LastError:      p.lastErr,
	}
	if info.PricePerMB > 0 {
		st.PricePerGB = tonwallet.FormatUnits(big.NewInt(info.PricePerMB*1024), 6)
		st.ChunkGB = float64(info.BytesFor(chunk)) / float64(1<<30)
	}
	p.status = st
}

// --- NodeApi surface ---

func (n *NodeApi) payStatePath() string {
	if n.node == nil || n.node.Cfg == nil || n.node.Cfg.Storage.DatabasePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(n.node.Cfg.Storage.DatabasePath), "ton", "autopay.json")
}

func (n *NodeApi) payInit() {
	n.pay.mu.Lock()
	defer n.pay.mu.Unlock()
	if n.pay.path == "" {
		n.pay.path = n.payStatePath()
	}
	n.pay.loadLocked()
}

// GetExitPayment describes what the connected exit costs and what is left of what was paid.
func (n *NodeApi) GetExitPayment() ExitPaymentStatus {
	n.payInit()
	n.pay.mu.Lock()
	defer n.pay.mu.Unlock()
	st := n.pay.status
	if n.pay.cancel == nil { // proxy is off: only the settings are meaningful
		st = ExitPaymentStatus{
			AutoPay:        n.pay.set.Enabled,
			ChunkUSDT:      tonwallet.FormatUnits(big.NewInt(n.pay.set.ChunkMicro), 6),
			DailyCapUSDT:   tonwallet.FormatUnits(big.NewInt(n.pay.set.DailyCapMicro), 6),
			SpentTodayUSDT: tonwallet.FormatUnits(big.NewInt(n.pay.set.Spent[day(n.pay.clock())]), 6),
			PayAsset:       n.pay.set.Asset,
		}
	}
	if rate := tonwallet.CachedUSDRate(); rate > 0 {
		chunk := n.pay.set.ChunkMicro
		st.ChunkTON = tonwallet.FormatTON(microUSDToTonNano(chunk, rate, tonPayMargin))
	}
	return st
}

// SetPayAsset chooses how traffic is paid: "auto", "TON" or "USDT".
func (n *NodeApi) SetPayAsset(asset string) error {
	switch asset {
	case PayAuto, PayTON, PayUSDT:
	default:
		return errors.New("pay with auto, TON or USDT")
	}
	n.payInit()
	n.pay.mu.Lock()
	defer n.pay.mu.Unlock()
	n.pay.set.Asset = asset
	n.pay.status.PayAsset = asset
	n.pay.saveLocked()
	return nil
}

// payExit pays micro-USD to the exit in the asset the settings and the wallet allow, and returns
// the asset used.
func (n *NodeApi) payExit(ctx context.Context, info *exit_gateway.AccountInfo, micro int64) (string, string, error) {
	w, err := n.ton()
	if err != nil {
		return "", "", err
	}
	n.payInit()
	n.pay.mu.Lock()
	pref := n.pay.set.Asset
	n.pay.mu.Unlock()

	tonBal, _, err := w.Balance(ctx)
	if err != nil {
		return "", "", fmt.Errorf("cannot read the wallet: %w", err)
	}
	var usdtBal *big.Int
	if info.Accepts(PayUSDT) && pref != PayTON {
		usdtBal, _ = w.TokenBalance(ctx, tonwallet.USDT) // not on testnet: then TON only
	}
	rate := 0.0
	if info.Accepts(PayTON) && pref != PayUSDT {
		rate, _ = tonwallet.USDRate(ctx)
	}
	choice, err := choosePayAsset(pref, *info, micro, usdtBal, tonBal, rate)
	if err != nil {
		return "", "", err
	}
	amount := tonwallet.FormatUnits(big.NewInt(micro), 6)
	if choice.Asset == PayTON {
		amount = tonwallet.FormatTON(choice.TonNano)
	}
	hash, err := n.PayForTraffic(ctx, choice.Asset, info.PayAddress, amount, info.Tag)
	return choice.Asset, hash, err
}

// SetAutoPay turns automatic top-ups on or off and sets their size and daily limit (USDT).
func (n *NodeApi) SetAutoPay(enabled bool, chunkUSDT, dailyCapUSDT string) error {
	chunk, err := tonwallet.ParseUnits(chunkUSDT, 6)
	if err != nil {
		return errors.New("enter the top-up amount in USDT, for example 1")
	}
	capMicro, err := tonwallet.ParseUnits(dailyCapUSDT, 6)
	if err != nil {
		return errors.New("enter the daily limit in USDT, for example 5")
	}
	if chunk.Int64() < exit_gateway.MinPayMicro || chunk.Int64() > maxManualPayMicro {
		return errors.New("a top-up must be between 0.01 and 200 USDT")
	}
	if capMicro.Cmp(chunk) < 0 {
		return errors.New("the daily limit cannot be smaller than one top-up")
	}
	n.payInit()
	n.pay.mu.Lock()
	defer n.pay.mu.Unlock()
	n.pay.set.Enabled, n.pay.set.ChunkMicro, n.pay.set.DailyCapMicro = enabled, chunk.Int64(), capMicro.Int64()
	if enabled {
		n.pay.blockedUntil = time.Time{} // the user is back and has just decided
	}
	n.pay.saveLocked()
	return nil
}

// PayExit pays the connected exit now, for an amount the user chose. It works whether or not
// automatic payments are on, and counts toward today's total.
func (n *NodeApi) PayExit(ctx context.Context, amountUSDT string) (string, error) {
	micro, err := tonwallet.ParseUnits(amountUSDT, 6)
	if err != nil {
		return "", err
	}
	if m := micro.Int64(); m < exit_gateway.MinPayMicro || m > maxManualPayMicro {
		return "", errors.New("pay between 0.01 and 200 dollars")
	}
	dialer, err := n.exitDialer()
	if err != nil {
		return "", err
	}
	info, err := dialer.QueryAccount(ctx)
	if err != nil {
		return "", fmt.Errorf("cannot reach the exit: %w", err)
	}
	if n.vaultUsable(ctx, info) {
		// paid from the vault: credited at once, nothing to wait for on-chain
		if err := n.payByVoucher(ctx, dialer, info, micro.Int64()); err != nil {
			return "", err
		}
		n.payInit()
		n.pay.mu.Lock()
		n.pay.set.Spent[day(n.pay.clock())] += micro.Int64()
		n.pay.lastErr = ""
		n.pay.saveLocked()
		n.pay.mu.Unlock()
		return "voucher", nil
	}
	if !info.CanPay() {
		return "", errors.New("this exit does not take payments")
	}
	_, hash, err := n.payExit(ctx, info, micro.Int64())
	if err != nil {
		return "", err
	}
	n.payInit()
	n.pay.mu.Lock()
	now := n.pay.clock()
	n.pay.set.Spent[day(now)] += micro.Int64()
	n.pay.pendingUntil = now.Add(payPendingFor)
	n.pay.creditBefore = info.Credit
	n.pay.lastErr = ""
	n.pay.saveLocked()
	n.pay.mu.Unlock()
	return hash, nil
}

// exitDialer returns a dialer to the exit the proxy is connected to.
func (n *NodeApi) exitDialer() (*exit_gateway.P2PDialer, error) {
	n.vpn.mu.Lock()
	relay := n.vpn.relayPeer
	running := n.vpn.server != nil
	n.vpn.mu.Unlock()
	if !running || n.node == nil || n.node.Swarm == nil {
		return nil, errors.New("connect to an exit first")
	}
	exit, err := n.relayPeerID(relay)
	if err != nil {
		return nil, err
	}
	return exit_gateway.NewP2PDialer(exit_gateway.SwarmOpener(n.node.Swarm, exit)), nil
}

// startPayGuard watches the exit's balance while the proxy runs.
func (n *NodeApi) startPayGuard() {
	dialer, err := n.exitDialer()
	if err != nil {
		return
	}
	n.vpn.mu.Lock()
	relay := n.vpn.relayPeer
	n.vpn.mu.Unlock()
	n.payInit()
	ctx, cancel := context.WithCancel(n.node.Ctx)
	n.pay.mu.Lock()
	if n.pay.cancel != nil {
		n.pay.cancel()
	}
	n.pay.cancel = cancel
	n.pay.status = ExitPaymentStatus{}
	n.pay.pendingUntil, n.pay.blockedUntil, n.pay.lastErr = time.Time{}, time.Time{}, ""
	n.pay.mu.Unlock()

	env := payEnv{
		query: func(ctx context.Context) (*exit_gateway.AccountInfo, error) {
			qctx, c := context.WithTimeout(ctx, 15*time.Second)
			defer c()
			info, err := dialer.QueryAccount(qctx)
			if ctx.Err() == nil {
				n.noteExitState(relay, info, err) // the first round runs right after connecting
			}
			return info, err
		},
		pay: func(ctx context.Context, micro int64, info *exit_gateway.AccountInfo) error {
			pctx, c := context.WithTimeout(ctx, 90*time.Second)
			defer c()
			amount := tonwallet.FormatUnits(big.NewInt(micro), 6)
			asset, _, err := n.payExit(pctx, info, micro)
			if err == nil {
				slog.Info("topped up the exit", "usd", amount, "asset", asset)
				n.emit("vpn:payment", map[string]any{"usd": amount, "asset": asset})
			}
			return err
		},
		vault: n.vaultUsable,
		payVoucher: func(ctx context.Context, micro int64, info *exit_gateway.AccountInfo) error {
			pctx, c := context.WithTimeout(ctx, 30*time.Second)
			defer c()
			return n.payByVoucher(pctx, dialer, info, micro)
		},
		traffic: func() uint64 {
			n.vpn.mu.Lock()
			defer n.vpn.mu.Unlock()
			if n.vpn.server == nil {
				return 0
			}
			_, rx, tx := n.vpn.server.Stats()
			return rx + tx
		},
	}
	go func() {
		for {
			wait := n.pay.step(ctx, env)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
}

func (n *NodeApi) stopPayGuard() {
	n.pay.mu.Lock()
	if n.pay.cancel != nil {
		n.pay.cancel()
		n.pay.cancel = nil
	}
	n.pay.status = ExitPaymentStatus{}
	n.pay.mu.Unlock()
}
