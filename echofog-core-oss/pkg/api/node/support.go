package node

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"sync"

	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
	"github.com/echofog/echofog-core/pkg/dto"
)

// supportFlushMu keeps two payments finishing together from sending the same pile twice.
var supportFlushMu sync.Mutex

func (n *NodeApi) support() *tonwallet.Support {
	if n.node == nil {
		return nil
	}
	return n.node.Support
}

// GetSupport describes the voluntary project share. When Available is false (no project address
// configured) the interface shows nothing about it.
func (n *NodeApi) GetSupport() dto.SupportDTO {
	s := n.support()
	if !s.Available() {
		return dto.SupportDTO{PendingTON: "0", PendingUSDT: "0"}
	}
	return dto.SupportDTO{
		Available:   true,
		Percent:     s.Percent(),
		PendingTON:  tonwallet.FormatTON(s.Pending("TON")),
		PendingUSDT: tonwallet.FormatUnits(s.Pending("USDT"), tonwallet.USDT.Decimals),
		Address:     s.Address(),
	}
}

// SetSupportPercent sets the share added on top of traffic payments, 0 to 20. Zero switches it
// off.
func (n *NodeApi) SetSupportPercent(percent int) error {
	s := n.support()
	if !s.Available() {
		return errors.New("project support is not configured")
	}
	return s.SetPercent(percent)
}

// GetOnRampURL returns the card top-up page for this wallet, or "" when no partner is set up.
func (n *NodeApi) GetOnRampURL() string {
	if n.node == nil || n.node.Cfg == nil {
		return ""
	}
	tpl := strings.TrimSpace(n.node.Cfg.Payments.OnRampURL)
	w, err := n.ton()
	if tpl == "" || err != nil {
		return ""
	}
	return strings.ReplaceAll(tpl, "{address}", w.Address())
}

// PayForTraffic pays a proxy node for traffic in "TON" or "USDT" and returns the message hash.
// The payment goes out in full; the voluntary project share is set aside on top and sent later in
// one batch (see tonwallet.Support). A failure to send the share never fails the payment.
func (n *NodeApi) PayForTraffic(ctx context.Context, asset, to, amount, memo string) (string, error) {
	w, err := n.ton()
	if err != nil {
		return "", err
	}
	asset = strings.ToUpper(strings.TrimSpace(asset))

	var (
		hash string
		paid *big.Int
	)
	switch asset {
	case "TON":
		if paid, err = tonwallet.ParseTON(amount); err != nil {
			return "", err
		}
		hash, err = w.Transfer(ctx, tonwallet.TransferRequest{To: to, Nano: paid, Comment: memo})
	case "USDT":
		if paid, err = tonwallet.ParseUnits(amount, tonwallet.USDT.Decimals); err != nil {
			return "", err
		}
		hash, err = w.TokenTransfer(ctx, tonwallet.USDT, to, paid, memo)
	default:
		return "", errors.New("pay in TON or USDT")
	}
	if err != nil {
		return "", err
	}

	if s := n.support(); s.Available() {
		if _, aerr := s.Accrue(asset, paid); aerr != nil {
			slog.Warn("could not record the project share", "err", aerr)
		}
		n.flushSupport(ctx, w, s, asset)
	}
	return hash, nil
}

// flushSupport sends the accrued share once it is worth the network fee. Any failure leaves it
// pending for the next payment.
func (n *NodeApi) flushSupport(ctx context.Context, w *tonwallet.Wallet, s *tonwallet.Support, asset string) {
	supportFlushMu.Lock()
	defer supportFlushMu.Unlock()

	due := s.Due(asset)
	if due == nil {
		return
	}
	var err error
	if asset == "TON" {
		_, err = w.Transfer(ctx, tonwallet.TransferRequest{To: s.Address(), Nano: due, Comment: tonwallet.SupportComment})
	} else {
		_, err = w.TokenTransfer(ctx, tonwallet.USDT, s.Address(), due, tonwallet.SupportComment)
	}
	if err != nil {
		slog.Info("project share stays pending", "asset", asset, "reason", err)
		return
	}
	if err := s.Settled(asset, due); err != nil {
		slog.Warn("project share was sent but not recorded", "err", err)
	}
}
