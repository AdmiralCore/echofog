package node

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
)

// earnState is the operator side: the watcher that books incoming USDT transfers while sharing.
type earnState struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

// tonPaymentSource reads the wallet's recent incoming USDT and TON transfers. The wallet already
// checks each token transfer against the real USDT jetton, so a look-alike token never shows up
// here as "USDT". TON is valued at the current TON/USD rate; without a rate a TON payment waits
// for the next round instead of being credited at a guess.
func tonPaymentSource(w *tonwallet.Wallet) exit_gateway.PaymentSource {
	return func(ctx context.Context) ([]exit_gateway.IncomingPayment, error) {
		cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		txs, err := w.History(cctx, 50)
		if err != nil {
			return nil, err
		}
		rate := 0.0
		for _, t := range txs {
			if t.Incoming && t.Asset == "TON" && t.Comment != "" {
				rate, _ = tonwallet.USDRate(cctx)
				break
			}
		}
		return paymentsFromTxs(txs, rate), nil
	}
}

// paymentsFromTxs keeps only what can pay for traffic: finished incoming USDT and TON transfers
// with a comment, valued in micro-USD (TON at tonUSD; with no rate TON is left for later).
// Outgoing, failed and look-alike-token transfers are dropped here.
func paymentsFromTxs(txs []tonwallet.Tx, tonUSD float64) []exit_gateway.IncomingPayment {
	out := make([]exit_gateway.IncomingPayment, 0, len(txs))
	for _, t := range txs {
		if !t.Incoming || t.Failed || t.Comment == "" {
			continue
		}
		amount, ok := new(big.Int).SetString(t.AmountNano, 10)
		if !ok || !amount.IsInt64() || amount.Sign() <= 0 {
			continue
		}
		var micro int64
		switch t.Asset {
		case tonwallet.USDT.Symbol:
			micro = amount.Int64()
		case "TON":
			if tonUSD <= 0 {
				continue
			}
			micro = tonNanoToMicroUSD(amount, tonUSD)
		default:
			continue
		}
		out = append(out, exit_gateway.IncomingPayment{Hash: t.Hash, Comment: t.Comment, Micro: micro, Time: t.Time, Asset: t.Asset})
	}
	return out
}

// tonNanoToMicroUSD values nano-TON in micro-USD: nano / 1e9 TON * rate USD * 1e6.
func tonNanoToMicroUSD(nano *big.Int, rate float64) int64 {
	v, _ := new(big.Float).Quo(new(big.Float).Mul(new(big.Float).SetInt(nano), big.NewFloat(rate)), big.NewFloat(1000)).Int64()
	return v
}

// microUSDToTonNano is the nano-TON that buys micro-USD at rate, with margin (0.02 = 2%) on top for
// the difference between the rate here and at the exit.
func microUSDToTonNano(micro int64, rate, margin float64) *big.Int {
	f := new(big.Float).Quo(new(big.Float).Mul(big.NewFloat(float64(micro)), big.NewFloat(1000*(1+margin))), big.NewFloat(rate))
	n, _ := f.Int(nil)
	return n.Add(n, big.NewInt(1))
}

// startEarning enables payments on the exit and starts booking transfers.
func (n *NodeApi) startEarning() error {
	if err := n.node.EnableExitPayments(); err != nil {
		return err
	}
	w, err := n.ton()
	if err != nil {
		return err
	}
	n.node.ExitService.OnPayment(func(micro, bytes int64) {
		n.emit("exit:payment", map[string]any{
			"usdt": tonwallet.FormatUnits(big.NewInt(micro), 6),
			"gb":   float64(bytes) / float64(1<<30),
		})
	})
	ctx, cancel := context.WithCancel(n.node.Ctx)
	n.earn.mu.Lock()
	if n.earn.cancel != nil {
		n.earn.cancel()
	}
	n.earn.cancel = cancel
	n.earn.mu.Unlock()
	go n.node.ExitService.WatchPayments(ctx, tonPaymentSource(w))
	if book := n.node.ExitVouchers(); book != nil {
		go n.runVoucherQuotes(ctx, book)
	}
	return nil
}

func (n *NodeApi) stopEarning() {
	n.earn.mu.Lock()
	if n.earn.cancel != nil {
		n.earn.cancel()
		n.earn.cancel = nil
	}
	n.earn.mu.Unlock()
}

// earnedUSDT is the total credited since the ledger was created.
func (n *NodeApi) earnedUSDT() string {
	if l := n.node.ExitLedger(); l != nil {
		return tonwallet.FormatUnits(big.NewInt(l.Earned()), 6)
	}
	return "0"
}
