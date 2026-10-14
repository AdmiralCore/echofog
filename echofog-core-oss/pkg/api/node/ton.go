package node

import (
	"context"
	"errors"
	"math/big"

	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
	"github.com/echofog/echofog-core/pkg/dto"
)

var errTonUnavailable = errors.New("TON wallet is not available, log in first")

func (n *NodeApi) ton() (*tonwallet.Wallet, error) {
	if n.node == nil || n.node.TonWallet == nil {
		return nil, errTonUnavailable
	}
	return n.node.TonWallet, nil
}

// GetTonWallet returns the wallet address and network; it works offline.
func (n *NodeApi) GetTonWallet() (dto.TonWalletDTO, error) {
	w, err := n.ton()
	if err != nil {
		return dto.TonWalletDTO{}, err
	}
	return dto.TonWalletDTO{
		Address:    w.Address(),
		RawAddress: w.RawAddress(),
		Network:    string(w.Network()),
		TopUpLink:  tonwallet.TopUpLink(w.Address(), nil, ""),
		Frozen:     w.IsFrozen(),
	}, nil
}

// GetTonBalance queries the confirmed balance from TON liteservers.
func (n *NodeApi) GetTonBalance(ctx context.Context) (dto.TonBalanceDTO, error) {
	w, err := n.ton()
	if err != nil {
		return dto.TonBalanceDTO{}, err
	}
	nano, deployed, err := w.Balance(ctx)
	if err != nil {
		return dto.TonBalanceDTO{}, err
	}
	out := dto.TonBalanceDTO{Nano: nano.String(), TON: tonwallet.FormatTON(nano), Deployed: deployed}
	if w.Network() == tonwallet.Mainnet {
		out.USDRate, _ = tonwallet.USDRate(ctx)
	}
	if usdt, err := w.TokenBalance(ctx, tonwallet.USDT); err == nil {
		out.USDTAvailable, out.USDT = true, tonwallet.FormatUnits(usdt, tonwallet.USDT.Decimals)
	}
	return out, nil
}

// GetTonHistory returns the latest wallet transactions, newest first.
func (n *NodeApi) GetTonHistory(ctx context.Context, limit int) ([]dto.TonTxDTO, error) {
	w, err := n.ton()
	if err != nil {
		return nil, err
	}
	txs, err := w.History(ctx, uint32(max(limit, 0)))
	if err != nil {
		return nil, err
	}
	out := make([]dto.TonTxDTO, 0, len(txs))
	for _, t := range txs {
		amount, _ := new(big.Int).SetString(t.AmountNano, 10)
		fee, _ := new(big.Int).SetString(t.FeeNano, 10)
		value := tonwallet.FormatTON(amount)
		if t.Asset == tonwallet.USDT.Symbol {
			value = tonwallet.FormatUnits(amount, tonwallet.USDT.Decimals)
		}
		out = append(out, dto.TonTxDTO{
			Asset: t.Asset, Hash: t.Hash, Time: t.Time.Unix(), Incoming: t.Incoming, TON: value,
			Counterparty: t.Counterparty, Comment: t.Comment, FeeTON: tonwallet.FormatTON(fee), Failed: t.Failed,
		})
	}
	return out, nil
}

// SendTon sends amountTON (decimal string, e.g. "0.5") to an address and returns the message hash.
func (n *NodeApi) SendTon(ctx context.Context, to, amountTON, comment string) (string, error) {
	w, err := n.ton()
	if err != nil {
		return "", err
	}
	nano, err := tonwallet.ParseTON(amountTON)
	if err != nil {
		return "", err
	}
	return w.Transfer(ctx, tonwallet.TransferRequest{To: to, Nano: nano, Comment: comment})
}

// SendUsdt sends a decimal USDT amount ("2.5") on TON; it needs ~0.06 TON for the network fee.
func (n *NodeApi) SendUsdt(ctx context.Context, to, amountUSDT, comment string) (string, error) {
	w, err := n.ton()
	if err != nil {
		return "", err
	}
	units, err := tonwallet.ParseUnits(amountUSDT, tonwallet.USDT.Decimals)
	if err != nil {
		return "", err
	}
	return w.TokenTransfer(ctx, tonwallet.USDT, to, units, comment)
}

// GetTonTopUpLink builds a ton:// link with an optional amount, for QR codes and wallet apps.
func (n *NodeApi) GetTonTopUpLink(amountTON string) (string, error) {
	w, err := n.ton()
	if err != nil {
		return "", err
	}
	var nano *big.Int
	if amountTON != "" {
		if nano, err = tonwallet.ParseTON(amountTON); err != nil {
			return "", err
		}
	}
	return tonwallet.TopUpLink(w.Address(), nano, ""), nil
}

// ExportTonRecoveryPhrase returns the 24-word TON phrase after re-checking the app password.
func (n *NodeApi) ExportTonRecoveryPhrase(appPassword string) ([]string, error) {
	if n.node == nil {
		return nil, errTonUnavailable
	}
	return n.node.TonRecoveryPhrase([]byte(appPassword))
}
