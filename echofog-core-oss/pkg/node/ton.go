package node

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/protection/core/identity"
	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
)

// initTonWallet binds the built-in TON wallet to the identity's wallet key. The address is
// derived offline; liteservers are contacted on first balance or transfer request.
func (n *Node) initTonWallet() {
	net := tonwallet.Network(n.Cfg.Ton.Network)
	if env := os.Getenv("ECHOFOG_TON_NETWORK"); env != "" {
		net = tonwallet.Network(env)
	}
	if net == "" {
		net = tonwallet.Mainnet
	}
	cacheDir := ""
	if n.Cfg.Storage.DatabasePath != "" {
		cacheDir = filepath.Join(filepath.Dir(n.Cfg.Storage.DatabasePath), "ton")
	}
	// The TON key comes from the standard 24-word TON phrase, which is itself derived from the
	// EchoFog wallet key: one backup restores both, and the phrase imports into TON wallets.
	phrase, err := tonwallet.DeriveRecoveryPhrase(n.identity)
	if err != nil {
		n.Logger.Warn("TON wallet unavailable", "err", err)
		return
	}
	signer, err := tonwallet.NewPhraseSigner(phrase)
	for i := range phrase {
		phrase[i] = ""
	}
	if err != nil {
		n.Logger.Warn("TON wallet unavailable", "err", err)
		return
	}
	w, err := tonwallet.New(signer, net, cacheDir)
	if err != nil {
		n.Logger.Warn("TON wallet unavailable", "err", err)
		return
	}
	n.TonWallet = w

	supportAddr := n.Cfg.Payments.SupportAddress
	if supportAddr != "" {
		// a typo here would send every tip into the void: refuse an address that does not parse
		// for this network
		if _, err := w.ParseRecipient(supportAddr); err != nil {
			n.Logger.Warn("support address ignored, it is not a valid TON address for this network", "err", err)
			supportAddr = ""
		}
	}
	n.Support = tonwallet.LoadSupport(cacheDir, supportAddr)
	n.Logger.Info("TON wallet ready", "network", net, "address", w.Address())

	// Vouchers signed to this node and not cashed yet are money: keep cashing them even when the
	// exit is not shared any more.
	if p := n.exitVoucherPath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			n.exitLedgerMu.Lock()
			n.openExitVouchersLocked()
			n.exitLedgerMu.Unlock()
		}
	}
}

func (n *Node) exitVoucherPath() string {
	if n.Cfg.Storage.DatabasePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(n.Cfg.Storage.DatabasePath), "ton", "exit-vouchers.json")
}

// openExitVouchersLocked opens the voucher book once and starts saving it and cashing what it
// holds for as long as the node runs.
func (n *Node) openExitVouchersLocked() *exit_gateway.VoucherBook {
	if n.exitVouchers == nil && n.TonWallet != nil {
		w := n.TonWallet
		n.exitVouchers = exit_gateway.NewVoucherBook(n.exitVoucherPath(), w.GlobalID(), w.WalletAddress(), w)
		n.exitVouchers.Start(n.Ctx)
		go n.exitVouchers.RunClaims(n.Ctx, exit_gateway.DefaultClaimPolicy())
	}
	return n.exitVouchers
}

// ExitVouchers returns the voucher book once it is open.
func (n *Node) ExitVouchers() *exit_gateway.VoucherBook {
	n.exitLedgerMu.Lock()
	defer n.exitLedgerMu.Unlock()
	return n.exitVouchers
}

// ErrWrongPassword is returned when a sensitive export is attempted with a bad app password.
var ErrWrongPassword = errors.New("wrong password")

// TonRecoveryPhrase re-checks the app password, then returns the 24-word TON phrase for export
// into another TON wallet.
func (n *Node) TonRecoveryPhrase(appPassword []byte) ([]string, error) {
	if n.identity == nil || n.Storage == nil {
		return nil, errors.New("log in first")
	}
	probe := identity.NewProtectedIdentity(n.Storage)
	if err := probe.Load(appPassword, false); err != nil {
		return nil, ErrWrongPassword
	}
	return tonwallet.DeriveRecoveryPhrase(n.identity)
}

// EnableExitPayments lets this node's exit take USDT for traffic: it opens the ledger (kept next to
// the database) and tells the exit which wallet clients pay into. Safe to call repeatedly.
func (n *Node) EnableExitPayments() error {
	if n.TonWallet == nil {
		return errors.New("the TON wallet is not available, log in first")
	}
	if n.ExitService == nil {
		return errors.New("node is not running")
	}
	n.exitLedgerMu.Lock()
	defer n.exitLedgerMu.Unlock()
	if n.exitLedger == nil {
		path := ""
		if n.Cfg.Storage.DatabasePath != "" {
			path = filepath.Join(filepath.Dir(n.Cfg.Storage.DatabasePath), "ton", "exit-ledger.json")
		}
		n.exitLedger = exit_gateway.NewLedger(path)
		n.exitLedger.Start()
	}
	n.ExitService.SetPayments(n.exitLedger, n.ID, n.TonWallet.Address())
	n.ExitService.SetVouchers(n.openExitVouchersLocked())
	return nil
}

// ExitLedger returns the ledger once payments are enabled.
func (n *Node) ExitLedger() *exit_gateway.Ledger {
	n.exitLedgerMu.Lock()
	defer n.exitLedgerMu.Unlock()
	return n.exitLedger
}

// closeExitLedger writes the ledger out when the node stops.
func (n *Node) closeExitLedger() {
	n.exitLedgerMu.Lock()
	l, v := n.exitLedger, n.exitVouchers
	n.exitLedgerMu.Unlock()
	if l != nil {
		l.Close()
	}
	if v != nil {
		_ = v.Flush()
	}
}
