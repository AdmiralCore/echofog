package node

// The user's EchoFogVault from the app: depositing, taking the deposit back, and paying the
// connected exit with vouchers from it instead of a transfer per chunk. The exit side (pricing in
// nanoton) is here too. See docs/vault-payments.md at the repository root.

import (
	"context"
	"encoding/hex"
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
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/xssnick/tonutils-go/address"
)

const (
	// voucherChunkMB: with vouchers nothing goes on-chain per chunk, so chunks can be small and
	// what is paid ahead stays at a fraction of a cent.
	voucherChunkMB = 64
	// voucherGuardEvery: the guard looks more often in voucher mode, so a fast download never
	// runs through the 16 MB left when the next chunk is bought.
	voucherGuardEvery = 3 * time.Second
	// voucherQuoteTolerance: the exit's nanoton price may be this much above ours (rates differ
	// a little between sources); more and the client pays by transfer instead.
	voucherQuoteTolerance = 0.05
	// ownVaultCheckEvery: how often the client re-reads its own vault while paying from it.
	ownVaultCheckEvery = time.Minute
	// quoteEvery: how often an exit re-prices vouchers from its USD price and the TON rate.
	quoteEvery = 10 * time.Minute
)

// vaultClient is the client's record of what it has signed to which exit. Only the latest total
// per exit matters; it is kept so a restart continues from it.
type vaultClient struct {
	mu        sync.Mutex
	path      string
	loaded    bool
	Signed    map[string]string `json:"signed"` // exit payout address (raw) -> cumulative nanoton
	info      tonwallet.VaultInfo
	checkedAt time.Time
}

func (n *NodeApi) vaultClientPath() string {
	if n.node == nil || n.node.Cfg == nil || n.node.Cfg.Storage.DatabasePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(n.node.Cfg.Storage.DatabasePath), "ton", "vouchers-signed.json")
}

func (v *vaultClient) loadLocked(path string) {
	if v.loaded {
		return
	}
	v.loaded, v.path = true, path
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(raw, v)
		}
	}
	if v.Signed == nil {
		v.Signed = map[string]string{}
	}
}

func (v *vaultClient) saveLocked() error {
	if v.path == "" {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(v.path), 0o700); err != nil {
		return err
	}
	tmp := v.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, v.path)
}

// ownVault reads this user's vault, at most once a minute unless fresh is set.
func (n *NodeApi) ownVault(ctx context.Context, fresh bool) (*address.Address, tonwallet.VaultInfo, error) {
	w, err := n.ton()
	if err != nil {
		return nil, tonwallet.VaultInfo{}, err
	}
	vault, err := w.Vault()
	if err != nil {
		return nil, tonwallet.VaultInfo{}, err
	}
	n.vault.mu.Lock()
	n.vault.loadLocked(n.vaultClientPath())
	cached, at := n.vault.info, n.vault.checkedAt
	n.vault.mu.Unlock()
	if !fresh && !at.IsZero() && time.Since(at) < ownVaultCheckEvery {
		return vault, cached, nil
	}
	info, err := w.ReadVault(ctx, vault)
	if err != nil {
		return vault, cached, err
	}
	n.vault.mu.Lock()
	n.vault.info, n.vault.checkedAt = info, time.Now()
	n.vault.mu.Unlock()
	return vault, info, nil
}

// fairQuote reports whether the exit's nanoton price matches its USD price at our TON rate.
func fairQuote(info exit_gateway.AccountInfo, rate float64) bool {
	if rate <= 0 || info.Vault == nil {
		return false
	}
	ceiling := microUSDToTonNano(info.PricePerMB, rate, voucherQuoteTolerance)
	return big.NewInt(info.Vault.NanoPerMB).Cmp(ceiling) <= 0
}

// vaultUsable tells whether this round pays the exit from the vault: the exit takes vouchers on
// our network at a fair price, and our vault holds money and is not being withdrawn.
func (n *NodeApi) vaultUsable(ctx context.Context, info *exit_gateway.AccountInfo) bool {
	if info == nil || !info.TakesVouchers() {
		return false
	}
	w, err := n.ton()
	if err != nil || info.Vault.GlobalID != w.GlobalID() {
		return false
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	rate, _ := tonwallet.USDRate(rctx)
	cancel()
	if !fairQuote(*info, rate) {
		return false
	}
	_, v, err := n.ownVault(ctx, false)
	if err != nil && v.Deposit == nil {
		return false
	}
	return v.Deployed && !v.Closing() && v.Deposit != nil && v.Deposit.Sign() > 0
}

// voucherNano is what micro-USD of traffic costs in nanoton at the exit's quote, rounded up so
// the exit credits at least the bytes it was bought for.
func voucherNano(info exit_gateway.AccountInfo, micro int64) *big.Int {
	bytes := big.NewInt(info.BytesFor(micro))
	nano := new(big.Int).Mul(bytes, big.NewInt(info.Vault.NanoPerMB))
	mb := big.NewInt(1 << 20)
	nano.Add(nano, new(big.Int).Sub(mb, big.NewInt(1)))
	return nano.Quo(nano, mb)
}

// payByVoucher buys micro-USD of traffic from the exit by raising what our vault allows it to take.
func (n *NodeApi) payByVoucher(ctx context.Context, dialer *exit_gateway.P2PDialer, info *exit_gateway.AccountInfo, micro int64) error {
	w, err := n.ton()
	if err != nil {
		return err
	}
	if !info.TakesVouchers() {
		return errors.New("this exit does not take vouchers")
	}
	node, err := address.ParseRawAddr(info.Vault.Node)
	if err != nil {
		return errors.New("the exit sent an invalid payout address")
	}
	nano := voucherNano(*info, micro)

	n.vault.mu.Lock()
	n.vault.loadLocked(n.vaultClientPath())
	prev, ok := new(big.Int).SetString(n.vault.Signed[node.StringRaw()], 10)
	if !ok {
		prev = new(big.Int)
	}
	n.vault.mu.Unlock()

	// The exit may hold a newer voucher than we remember (we lost our file, or use two
	// devices). Take it only if it really carries our signature: an exit cannot make us sign on
	// top of an amount we never agreed to.
	if held, ok := new(big.Int).SetString(info.Vault.Cumulative, 10); ok && held.Cmp(prev) > 0 {
		sig, _ := hex.DecodeString(info.Vault.Sig)
		vault, verr := w.Vault()
		if verr == nil && tonwallet.VerifyVoucher(w.PublicKey(), tonwallet.Voucher{GlobalID: w.GlobalID(), Vault: vault, Node: node, Cumulative: held}, sig) {
			prev = held
		}
	}
	next := new(big.Int).Add(prev, nano)
	sv, err := w.SignVoucher(node, next)
	if err != nil {
		return err
	}
	// Saved before it is sent. If sending fails the next voucher simply adds to this total: the
	// exit credits the whole difference to what it holds, so nothing is paid twice.
	n.vault.mu.Lock()
	n.vault.Signed[node.StringRaw()] = next.String()
	serr := n.vault.saveLocked()
	n.vault.mu.Unlock()
	if serr != nil {
		return fmt.Errorf("cannot record the voucher: %w", serr)
	}

	reply, err := dialer.SendVoucher(ctx, exit_gateway.VoucherOffer{
		Version:    1,
		Owner:      w.WalletAddress().StringRaw(),
		PubKey:     hex.EncodeToString(w.PublicKey()),
		Cumulative: next.String(),
		Sig:        hex.EncodeToString(sv.Signature),
	})
	if err != nil {
		return fmt.Errorf("cannot reach the exit: %w", err)
	}
	if !reply.OK {
		n.vault.mu.Lock()
		n.vault.checkedAt = time.Time{} // re-read the vault: the exit may know something we do not
		n.vault.mu.Unlock()
		return errors.New(reply.Error)
	}
	if s := n.support(); s.Available() {
		if _, aerr := s.Accrue("TON", nano); aerr != nil {
			slog.Warn("could not record the project share", "err", aerr)
		}
		n.flushSupport(ctx, w, s, "TON")
	}
	return nil
}

// --- the exit side: pricing vouchers ---

// runVoucherQuotes keeps the exit's nanoton price in step with its USD price and the TON rate,
// until ctx ends. Without a rate it takes no vouchers rather than guess.
func (n *NodeApi) runVoucherQuotes(ctx context.Context, book *exit_gateway.VoucherBook) {
	defer book.SetQuote(0)
	for {
		price := n.node.ExitService.CurrentPolicy().PricePerMB
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		rate, err := tonwallet.USDRate(rctx)
		cancel()
		switch {
		case price <= 0:
			book.SetQuote(0)
		case err == nil && rate > 0:
			// micro-USD per MB -> nanoton per MB: micro / 1e6 USD / rate TON * 1e9
			book.SetQuote(max(1, int64(float64(price)*1000/rate)))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(quoteEvery):
		}
	}
}

// GetVoucherEarnings reports what this node's exit earned from vouchers.
func (n *NodeApi) GetVoucherEarnings() dto.VoucherEarningsDTO {
	out := dto.VoucherEarningsDTO{OwedTON: "0", CashedTON: "0"}
	if n.node == nil {
		return out
	}
	if b := n.node.ExitVouchers(); b != nil {
		out.OwedTON = tonwallet.FormatTON(b.Owed())
		out.CashedTON = tonwallet.FormatTON(b.Cashed())
		out.QuoteNano = b.Quote()
	}
	return out
}

// --- the user's vault ---

// GetVault describes the user's vault.
func (n *NodeApi) GetVault(ctx context.Context) (dto.VaultDTO, error) {
	w, err := n.ton()
	if err != nil {
		return dto.VaultDTO{}, err
	}
	vault, info, err := n.ownVault(ctx, true)
	if err != nil {
		return dto.VaultDTO{}, err
	}
	out := dto.VaultDTO{
		Address:     w.FriendlyAddress(vault),
		Deployed:    info.Deployed,
		DepositTON:  "0",
		Closing:     info.Closing(),
		MinFirstTON: tonwallet.FormatTON(tonwallet.VaultMinFirstDeposit),
	}
	if info.Deposit != nil {
		out.DepositTON = tonwallet.FormatTON(info.Deposit)
	}
	if info.Closing() {
		out.WithdrawAt = info.WithdrawAt.Unix()
		out.CanFinish = !info.ChainNow.Before(info.WithdrawAt)
	}
	return out, nil
}

func (n *NodeApi) vaultChanged() {
	n.vault.mu.Lock()
	n.vault.checkedAt = time.Time{}
	n.vault.mu.Unlock()
}

// VaultDeposit moves a decimal TON amount from the wallet into the vault, creating it the first time.
func (n *NodeApi) VaultDeposit(ctx context.Context, amountTON string) (string, error) {
	w, err := n.ton()
	if err != nil {
		return "", err
	}
	nano, err := tonwallet.ParseTON(amountTON)
	if err != nil {
		return "", err
	}
	defer n.vaultChanged()
	return w.VaultDeposit(ctx, nano)
}

// VaultRequestWithdraw starts the 24-hour countdown to taking the deposit back.
func (n *NodeApi) VaultRequestWithdraw(ctx context.Context) (string, error) {
	w, err := n.ton()
	if err != nil {
		return "", err
	}
	defer n.vaultChanged()
	return w.VaultRequestWithdraw(ctx)
}

// VaultCancelWithdraw stops a pending withdrawal.
func (n *NodeApi) VaultCancelWithdraw(ctx context.Context) (string, error) {
	w, err := n.ton()
	if err != nil {
		return "", err
	}
	defer n.vaultChanged()
	return w.VaultCancelWithdraw(ctx)
}

// VaultExecuteWithdraw sends the remaining deposit back to the wallet once the 24 hours are over.
func (n *NodeApi) VaultExecuteWithdraw(ctx context.Context) (string, error) {
	w, err := n.ton()
	if err != nil {
		return "", err
	}
	defer n.vaultChanged()
	return w.VaultExecuteWithdraw(ctx)
}
