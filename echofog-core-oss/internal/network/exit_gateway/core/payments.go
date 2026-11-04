package exit_gateway

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// IncomingPayment is one transfer received by the exit's wallet, valued in micro-USD: USDT as it
// is, TON at the rate when it arrived.
type IncomingPayment struct {
	Hash    string
	Comment string
	Micro   int64 // micro-USD
	Time    time.Time
	Asset   string // "USDT" or "TON"
}

// PaymentSource returns the wallet's recent incoming USDT transfers (already checked against the
// real USDT jetton, so a look-alike token cannot be credited).
type PaymentSource func(ctx context.Context) ([]IncomingPayment, error)

const (
	paymentPollEvery   = 6 * time.Second
	paymentPollBackoff = 30 * time.Second
	payTagPrefix       = "ef1:"
)

// ProcessPayments books every new valid transfer and returns how many it credited. Anything with
// a foreign comment, below the dust limit, already booked, or too old is ignored, so a transfer is
// only ever credited once however often the history is read.
func (e *ExitService) ProcessPayments(list []IncomingPayment) int {
	e.policyMu.Lock()
	l, price := e.ledger, e.policy.PricePerMB
	e.policyMu.Unlock()
	if l == nil || price <= 0 {
		return 0
	}
	credited := 0
	for _, p := range list {
		if !strings.HasPrefix(p.Comment, payTagPrefix) || len(p.Comment) != len(payTagPrefix)+16 {
			continue
		}
		if l.Seen(p.Hash, p.Time) {
			continue
		}
		if bytes, ok := l.Credit(p.Comment, p.Hash, p.Micro, price); ok {
			credited++
			slog.Info("exit payment credited", "asset", p.Asset, "micro_usd", p.Micro, "mb", bytes/bytesPerMB)
			if cb := e.onPayment.Load(); cb != nil {
				(*cb)(p.Micro, bytes)
			}
		}
	}
	return credited
}

// OnPayment registers a callback fired for every credited transfer (micro-USDT, bytes bought).
func (e *ExitService) OnPayment(f func(micro, bytes int64)) { e.onPayment.Store(&f) }

// WatchPayments polls src until ctx ends and books incoming transfers. A failing source backs off
// instead of hammering the TON liteservers.
func (e *ExitService) WatchPayments(ctx context.Context, src PaymentSource) {
	e.watchEvery(ctx, src, paymentPollEvery, paymentPollBackoff)
}

func (e *ExitService) watchEvery(ctx context.Context, src PaymentSource, every, backoff time.Duration) {
	wait := every
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		list, err := src(ctx)
		if err != nil {
			wait = backoff
			slog.Debug("payment check failed", "err", err)
			continue
		}
		wait = every
		e.ProcessPayments(list)
	}
}
