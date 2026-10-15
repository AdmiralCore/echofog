package main

import (
	"context"
	"time"

	"github.com/echofog/echofog-core/pkg/dto"
)

// --- BUILT-IN TON WALLET ---

func (a *App) tonContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	a.mu.RLock()
	parent := a.ctx
	a.mu.RUnlock()
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout)
}

// GetTonWallet returns the TON address and network of the built-in wallet.
func (a *App) GetTonWallet() (dto.TonWalletDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return dto.TonWalletDTO{}, errNodeNotStarted
	}
	return nodeAPI.GetTonWallet()
}

// GetTonBalance reads the confirmed balance from TON liteservers.
func (a *App) GetTonBalance() (dto.TonBalanceDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return dto.TonBalanceDTO{}, errNodeNotStarted
	}
	ctx, cancel := a.tonContext(30 * time.Second)
	defer cancel()
	return nodeAPI.GetTonBalance(ctx)
}

// GetTonHistory returns recent wallet transactions, newest first.
func (a *App) GetTonHistory(limit int) ([]dto.TonTxDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil, errNodeNotStarted
	}
	ctx, cancel := a.tonContext(30 * time.Second)
	defer cancel()
	return nodeAPI.GetTonHistory(ctx, limit)
}

// SendTon sends a decimal TON amount ("0.5") with an optional comment; returns the message hash.
func (a *App) SendTon(to string, amountTON string, comment string) (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errNodeNotStarted
	}
	if err := a.confirmSendAboveThreshold("TON", amountTON, to); err != nil {
		return "", err
	}
	ctx, cancel := a.tonContext(45 * time.Second)
	defer cancel()
	return nodeAPI.SendTon(ctx, to, amountTON, comment)
}

// GetTonTopUpLink builds a ton:// transfer link (with an optional amount) for the receive QR.
func (a *App) GetTonTopUpLink(amountTON string) (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errNodeNotStarted
	}
	return nodeAPI.GetTonTopUpLink(amountTON)
}

// SendUsdt sends USDT on TON (needs a little TON for the fee); returns the message hash.
func (a *App) SendUsdt(to string, amountUSDT string, comment string) (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errNodeNotStarted
	}
	if err := a.confirmSendAboveThreshold("USDT", amountUSDT, to); err != nil {
		return "", err
	}
	ctx, cancel := a.tonContext(45 * time.Second)
	defer cancel()
	return nodeAPI.SendUsdt(ctx, to, amountUSDT, comment)
}

// ExportTonRecoveryPhrase returns the 24-word TON phrase after re-checking the app password.
func (a *App) ExportTonRecoveryPhrase(appPassword string) ([]string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil, errNodeNotStarted
	}
	return nodeAPI.ExportTonRecoveryPhrase(appPassword)
}

// GetSupport returns the voluntary project share of traffic payments (Available=false: hidden).
func (a *App) GetSupport() dto.SupportDTO {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return dto.SupportDTO{PendingTON: "0", PendingUSDT: "0"}
	}
	return nodeAPI.GetSupport()
}

// SetSupportPercent sets the share (0-20); 0 switches it off.
func (a *App) SetSupportPercent(percent int) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	return nodeAPI.SetSupportPercent(percent)
}

// GetOnRampURL returns the card top-up page for this wallet, or "" when no partner is set up.
func (a *App) GetOnRampURL() string {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return ""
	}
	return nodeAPI.GetOnRampURL()
}

// PayForTraffic pays a proxy node in TON or USDT; the project share is added on top if enabled.
func (a *App) PayForTraffic(asset, to, amount, memo string) (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errNodeNotStarted
	}
	if err := a.confirmSendAboveThreshold(asset, amount, to); err != nil {
		return "", err
	}
	ctx, cancel := a.tonContext(90 * time.Second)
	defer cancel()
	return nodeAPI.PayForTraffic(ctx, asset, to, amount, memo)
}

// --- TON VAULT: the deposit paid proxy nodes are paid from with vouchers ---

// GetVault describes the user's vault: address, deposit, and any pending withdrawal.
func (a *App) GetVault() (dto.VaultDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return dto.VaultDTO{}, errNodeNotStarted
	}
	ctx, cancel := a.tonContext(30 * time.Second)
	defer cancel()
	return nodeAPI.GetVault(ctx)
}

// VaultDeposit moves a decimal TON amount from the wallet into the vault; returns the message hash.
func (a *App) VaultDeposit(amountTON string) (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errNodeNotStarted
	}
	if err := a.confirmSendAboveThreshold("TON", amountTON, "your vault"); err != nil {
		return "", err
	}
	ctx, cancel := a.tonContext(45 * time.Second)
	defer cancel()
	return nodeAPI.VaultDeposit(ctx, amountTON)
}

// VaultRequestWithdraw starts the 24-hour countdown to taking the deposit back.
func (a *App) VaultRequestWithdraw() (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errNodeNotStarted
	}
	ctx, cancel := a.tonContext(45 * time.Second)
	defer cancel()
	return nodeAPI.VaultRequestWithdraw(ctx)
}

// VaultCancelWithdraw stops a pending withdrawal.
func (a *App) VaultCancelWithdraw() (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errNodeNotStarted
	}
	ctx, cancel := a.tonContext(45 * time.Second)
	defer cancel()
	return nodeAPI.VaultCancelWithdraw(ctx)
}

// VaultExecuteWithdraw returns the remaining deposit to the wallet once the 24 hours are over.
func (a *App) VaultExecuteWithdraw() (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errNodeNotStarted
	}
	// No single amount here (it returns the whole deposit), so always confirm this money move.
	if err := a.confirmNative("Confirm withdrawal", "Return the vault deposit to your wallet?"); err != nil {
		return "", err
	}
	ctx, cancel := a.tonContext(45 * time.Second)
	defer cancel()
	return nodeAPI.VaultExecuteWithdraw(ctx)
}

// GetVoucherEarnings reports what this node's exit earned from vouchers (owed and cashed).
func (a *App) GetVoucherEarnings() dto.VoucherEarningsDTO {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return dto.VoucherEarningsDTO{OwedTON: "0", CashedTON: "0"}
	}
	return nodeAPI.GetVoucherEarnings()
}
