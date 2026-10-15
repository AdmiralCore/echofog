package tonwallet

// The EchoFogVault contract (echofog-core-oss/contracts/vault) from the Go side.
//
// Each user who pays for traffic has their own vault: a small contract holding their deposit.
// The app signs vouchers off-chain, "node N may take, in total, X nanoton", and the node cashes
// the latest one when it is worth the gas. The voucher bytes built here must be exactly what the
// contract parses; voucher_test.go checks them against a fixture made by the contract tests.

import (
	"context"
	"crypto/ed25519"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// vault_code.boc is the compiled contract, copied here by `npm run build` in contracts/vault. Its
// hash is what makes a vault address "the real EchoFogVault": a node derives the address itself
// from this code and the user's (owner, key), so it never has to trust a contract it is shown.
//
//go:embed vault_code.boc
var vaultCodeBOC []byte

const (
	// VoucherMagic opens every voucher cell ("EFV1"). The same ed25519 key signs wallet
	// transfers, whose signed cells start differently, so one can never pass for the other.
	VoucherMagic = 0x45465631

	// TON global IDs (config param 19). A voucher names its network, so a testnet voucher can
	// not be cashed on mainnet even though the vault address is the same on both.
	MainnetGlobalID int32 = -239
	TestnetGlobalID int32 = -3

	opVaultDeposit         = 0x6566d001
	opVaultClaim           = 0x6566d002
	opVaultRequestWithdraw = 0x6566d003
	opVaultCancelWithdraw  = 0x6566d004
	opVaultExecuteWithdraw = 0x6566d005

	// VaultCooldown is how long the owner waits between asking for the deposit back and
	// getting it. Nodes cash what they were signed for meanwhile.
	VaultCooldown = 24 * time.Hour

	// MaxClaimsPerSend stays under the wallet v5 limit of 255 messages per transfer.
	MaxClaimsPerSend = 200
)

// Vault exit codes, as the contract throws them.
const (
	VaultErrBadVoucher    = 1000
	VaultErrWrongNetwork  = 1001
	VaultErrWrongVault    = 1002
	VaultErrBadSignature  = 1003
	VaultErrNothingToPay  = 1004
	VaultErrEmptyDeposit  = 1005
	VaultErrNotOwner      = 1006
	VaultErrNoWithdraw    = 1007
	VaultErrCooldown      = 1008
	VaultErrLowGas        = 1009
	VaultErrDepositTooLow = 1010
)

var (
	// VaultClaimGas is attached to each claim; the contract needs at least 0.01 TON and sends
	// what it does not use back with the payout. A claim really costs about 0.0012 TON.
	VaultClaimGas = big.NewInt(15_000_000)

	// VaultOpGas is attached to the owner's withdraw messages; the unused part comes back.
	VaultOpGas = big.NewInt(20_000_000)

	// VaultMinFirstDeposit: the first deposit also creates the contract, which keeps about
	// 0.008 TON for its storage and gas; less than this would leave almost nothing to spend.
	VaultMinFirstDeposit = big.NewInt(100_000_000) // 0.1 TON

	ErrVaultCooldown   = errors.New("the vault is being withdrawn: wait until the 24 hours are over")
	ErrVaultNoWithdraw = errors.New("no withdrawal was requested")
	ErrVaultNotFound   = errors.New("the vault has not been created yet: make a first deposit")
	ErrBadVoucher      = errors.New("invalid voucher")
)

var (
	vaultCodeOnce sync.Once
	vaultCode     *cell.Cell
	vaultCodeErr  error
)

// VaultCode is the compiled EchoFogVault code cell.
func VaultCode() (*cell.Cell, error) {
	vaultCodeOnce.Do(func() {
		vaultCode, vaultCodeErr = cell.FromBOC(vaultCodeBOC)
	})
	return vaultCode, vaultCodeErr
}

// GlobalIDOf is the global ID of a TON network.
func GlobalIDOf(net Network) int32 {
	if net == Testnet {
		return TestnetGlobalID
	}
	return MainnetGlobalID
}

// VaultStateInit is the vault of (owner, pub) as it is first deployed: the contract code and
// owner | pubKey | deposit 0 | withdrawAt 0 | no claims.
func VaultStateInit(owner *address.Address, pub ed25519.PublicKey) (*tlb.StateInit, error) {
	if owner == nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("vault needs an owner address and an ed25519 key")
	}
	code, err := VaultCode()
	if err != nil {
		return nil, fmt.Errorf("vault code: %w", err)
	}
	b := cell.BeginCell()
	if err := firstErr(
		b.StoreAddr(owner),
		b.StoreSlice(pub, 256),
		b.StoreBigCoins(new(big.Int)),
		b.StoreUInt(0, 32),
		b.StoreMaybeRef(nil), // empty claimed map
	); err != nil {
		return nil, err
	}
	return &tlb.StateInit{Code: code, Data: b.EndCell()}, nil
}

// VaultAddress is the address of (owner, pub)'s vault on the basechain.
func VaultAddress(owner *address.Address, pub ed25519.PublicKey) (*address.Address, error) {
	si, err := VaultStateInit(owner, pub)
	if err != nil {
		return nil, err
	}
	c, err := tlb.ToCell(si)
	if err != nil {
		return nil, err
	}
	return address.NewAddress(0, 0, c.Hash()).Bounce(true), nil
}

// Voucher allows Node to take Cumulative nanoton, in total over all time, from Vault.
type Voucher struct {
	GlobalID   int32
	Vault      *address.Address
	Node       *address.Address
	Cumulative *big.Int
}

// Cell is the voucher as the contract parses it.
func (v Voucher) Cell() (*cell.Cell, error) {
	if v.Vault == nil || v.Node == nil || v.Cumulative == nil || v.Cumulative.Sign() < 0 {
		return nil, ErrBadVoucher
	}
	b := cell.BeginCell()
	if err := firstErr(
		b.StoreUInt(VoucherMagic, 32),
		b.StoreInt(int64(v.GlobalID), 32),
		b.StoreAddr(v.Vault),
		b.StoreAddr(v.Node),
		b.StoreBigCoins(v.Cumulative), // fails above 2^120: such a voucher cannot exist on chain
	); err != nil {
		return nil, ErrBadVoucher
	}
	return b.EndCell(), nil
}

// firstErr returns the first error of a chain of builder stores.
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// Hash is what gets signed: the hash of Cell, which TVM CHKSIGNU checks.
func (v Voucher) Hash() ([]byte, error) {
	c, err := v.Cell()
	if err != nil {
		return nil, err
	}
	return c.Hash(), nil
}

// SignedVoucher is a voucher with its owner's signature, ready to be claimed.
type SignedVoucher struct {
	Voucher
	Signature []byte
}

// VerifyVoucher checks that pub signed v.
func VerifyVoucher(pub ed25519.PublicKey, v Voucher, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	h, err := v.Hash()
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, h, sig)
}

// SignVoucherWith signs v with signer (the identity boundary, or a test key).
func SignVoucherWith(signer Signer, v Voucher) (SignedVoucher, error) {
	h, err := v.Hash()
	if err != nil {
		return SignedVoucher{}, err
	}
	sig, err := signer.SignWalletTransaction(h)
	if err != nil {
		return SignedVoucher{}, fmt.Errorf("sign voucher: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return SignedVoucher{}, errors.New("signer returned a malformed signature")
	}
	return SignedVoucher{Voucher: v, Signature: sig}, nil
}

// ClaimBody is the Claim message that cashes sv.
func ClaimBody(sv SignedVoucher, queryID uint64) (*cell.Cell, error) {
	vc, err := sv.Cell()
	if err != nil {
		return nil, err
	}
	if len(sv.Signature) != ed25519.SignatureSize {
		return nil, ErrBadVoucher
	}
	b := cell.BeginCell()
	if err := firstErr(
		b.StoreUInt(opVaultClaim, 32),
		b.StoreUInt(queryID, 64),
		b.StoreRef(vc),
		b.StoreSlice(sv.Signature, 512),
	); err != nil {
		return nil, err
	}
	return b.EndCell(), nil
}

func vaultOpBody(op uint32, queryID uint64) *cell.Cell {
	return cell.BeginCell().MustStoreUInt(uint64(op), 32).MustStoreUInt(queryID, 64).EndCell()
}

// VaultInfo is what the chain says about a vault right now.
type VaultInfo struct {
	Deployed   bool
	Deposit    *big.Int  // nanoton nodes can still be paid from
	WithdrawAt time.Time // zero: no withdrawal pending
	ChainNow   time.Time
}

// Closing reports whether the owner asked for the deposit back: nodes should not sell against it.
func (v VaultInfo) Closing() bool { return !v.WithdrawAt.IsZero() }

// ---- Wallet side ----

// GlobalID is the global ID of the wallet's network.
func (w *Wallet) GlobalID() int32 { return GlobalIDOf(w.net) }

// PublicKey is the key that signs transfers and vouchers.
func (w *Wallet) PublicKey() ed25519.PublicKey { return append(ed25519.PublicKey(nil), w.pub...) }

// WalletAddress is the wallet contract address.
func (w *Wallet) WalletAddress() *address.Address { return w.addr }

// Vault is this wallet's own vault address.
func (w *Wallet) Vault() (*address.Address, error) { return VaultAddress(w.addr, w.pub) }

// FriendlyAddress formats a for this wallet's network (bounceable: a deployed contract).
func (w *Wallet) FriendlyAddress(a *address.Address) string {
	return a.Bounce(true).Testnet(w.net == Testnet).String()
}

// SignVoucher signs "node may take cumulative nanoton from my vault, in total". A frozen wallet
// signs nothing, since a voucher is money as much as a transfer is.
func (w *Wallet) SignVoucher(node *address.Address, cumulative *big.Int) (SignedVoucher, error) {
	if w.IsFrozen() {
		return SignedVoucher{}, ErrFrozen
	}
	vault, err := w.Vault()
	if err != nil {
		return SignedVoucher{}, err
	}
	return SignVoucherWith(w.signer, Voucher{GlobalID: w.GlobalID(), Vault: vault, Node: node, Cumulative: cumulative})
}

// ReadVault reads a vault's state from the chain.
func (w *Wallet) ReadVault(ctx context.Context, vault *address.Address) (VaultInfo, error) {
	api, _, err := w.connect(ctx)
	if err != nil {
		return VaultInfo{}, err
	}
	block, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return VaultInfo{}, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	return readVault(ctx, api, block, vault)
}

func readVault(ctx context.Context, api ton.APIClientWrapped, block *ton.BlockIDExt, vault *address.Address) (VaultInfo, error) {
	acc, err := api.GetAccount(ctx, block, vault)
	if err != nil {
		return VaultInfo{}, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	if !acc.IsActive || acc.State == nil || acc.State.Status != tlb.AccountStatusActive {
		return VaultInfo{Deposit: new(big.Int)}, nil
	}
	res, err := api.RunGetMethod(ctx, block, vault, "state")
	if err != nil {
		return VaultInfo{}, fmt.Errorf("vault state: %w", err)
	}
	return parseVaultState(res.AsTuple())
}

// parseVaultState reads the `state` getter: owner, pubKey, deposit, withdrawAt, now.
func parseVaultState(stack []any) (VaultInfo, error) {
	if len(stack) < 5 {
		return VaultInfo{}, errors.New("vault state: unexpected getter result")
	}
	num := func(i int) (*big.Int, error) {
		v, ok := stack[i].(*big.Int)
		if !ok {
			return nil, errors.New("vault state: unexpected getter result")
		}
		return v, nil
	}
	dep, err := num(2)
	if err != nil {
		return VaultInfo{}, err
	}
	at, err := num(3)
	if err != nil {
		return VaultInfo{}, err
	}
	now, err := num(4)
	if err != nil {
		return VaultInfo{}, err
	}
	info := VaultInfo{Deployed: true, Deposit: new(big.Int).Set(dep), ChainNow: time.Unix(now.Int64(), 0)}
	if at.Sign() > 0 {
		info.WithdrawAt = time.Unix(at.Int64(), 0)
	}
	return info, nil
}

// VaultClaimed is how much node has already been paid from vault.
func (w *Wallet) VaultClaimed(ctx context.Context, vault, node *address.Address) (*big.Int, error) {
	api, _, err := w.connect(ctx)
	if err != nil {
		return nil, err
	}
	block, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	acc, err := api.GetAccount(ctx, block, vault)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	if !acc.IsActive || acc.State == nil || acc.State.Status != tlb.AccountStatusActive {
		return new(big.Int), nil
	}
	arg, err := cell.BeginCell().MustStoreAddr(node).EndCell().BeginParse()
	if err != nil {
		return nil, err
	}
	res, err := api.RunGetMethod(ctx, block, vault, "claimedBy", arg)
	if err != nil {
		return nil, fmt.Errorf("vault claimedBy: %w", err)
	}
	v, err := res.Int(0)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// send broadcasts messages from the wallet after the frozen and balance checks, and returns the
// external message hash.
func (w *Wallet) send(ctx context.Context, msgs []*wallet.Message, total *big.Int) (string, error) {
	if w.IsFrozen() {
		return "", ErrFrozen
	}
	bal, _, err := w.Balance(ctx)
	if err != nil {
		return "", err
	}
	if new(big.Int).Add(total, FeeReserve).Cmp(bal) > 0 {
		return "", ErrInsufficientFunds
	}
	_, tw, err := w.connect(ctx)
	if err != nil {
		return "", err
	}
	hash, err := tw.SendManyGetInMsgHash(ctx, msgs, false)
	if err != nil {
		return "", fmt.Errorf("broadcast failed: %w", err)
	}
	return hex.EncodeToString(hash), nil
}

func internalMsg(to *address.Address, nano *big.Int, bounce bool, body *cell.Cell, si *tlb.StateInit) *wallet.Message {
	return &wallet.Message{
		Mode: wallet.PayGasSeparately + wallet.IgnoreErrors,
		InternalMessage: &tlb.InternalMessage{
			IHRDisabled: true,
			Bounce:      bounce,
			DstAddr:     to,
			Amount:      tlb.FromNanoTON(nano),
			Body:        body,
			StateInit:   si,
		},
	}
}

// VaultDeposit adds nano to this wallet's vault, creating the vault with the first deposit.
func (w *Wallet) VaultDeposit(ctx context.Context, nano *big.Int) (string, error) {
	if nano == nil || nano.Sign() <= 0 {
		return "", ErrBadAmount
	}
	vault, err := w.Vault()
	if err != nil {
		return "", err
	}
	info, err := w.ReadVault(ctx, vault)
	if err != nil {
		return "", err
	}
	var si *tlb.StateInit
	if !info.Deployed {
		if nano.Cmp(VaultMinFirstDeposit) < 0 {
			return "", fmt.Errorf("the first deposit must be at least %s TON: it also creates the vault", FormatTON(VaultMinFirstDeposit))
		}
		if si, err = VaultStateInit(w.addr, w.pub); err != nil {
			return "", err
		}
	}
	// Not bounceable while it does not exist yet: the deploy must not bounce back on its way in.
	return w.send(ctx, []*wallet.Message{internalMsg(vault, nano, info.Deployed, vaultOpBody(opVaultDeposit, uint64(time.Now().UnixNano())), si)}, nano)
}

func (w *Wallet) vaultOwnerOp(ctx context.Context, op uint32) (string, error) {
	vault, err := w.Vault()
	if err != nil {
		return "", err
	}
	info, err := w.ReadVault(ctx, vault)
	if err != nil {
		return "", err
	}
	if !info.Deployed {
		return "", ErrVaultNotFound
	}
	switch op {
	case opVaultExecuteWithdraw:
		if !info.Closing() {
			return "", ErrVaultNoWithdraw
		}
		if info.ChainNow.Before(info.WithdrawAt) {
			return "", ErrVaultCooldown
		}
	case opVaultCancelWithdraw:
		if !info.Closing() {
			return "", ErrVaultNoWithdraw
		}
	}
	return w.send(ctx, []*wallet.Message{internalMsg(vault, VaultOpGas, true, vaultOpBody(op, uint64(time.Now().UnixNano())), nil)}, VaultOpGas)
}

// VaultRequestWithdraw starts the 24-hour countdown to getting the deposit back.
func (w *Wallet) VaultRequestWithdraw(ctx context.Context) (string, error) {
	return w.vaultOwnerOp(ctx, opVaultRequestWithdraw)
}

// VaultCancelWithdraw stops a pending withdrawal, so nodes take the vault again.
func (w *Wallet) VaultCancelWithdraw(ctx context.Context) (string, error) {
	return w.vaultOwnerOp(ctx, opVaultCancelWithdraw)
}

// VaultExecuteWithdraw returns the whole remaining deposit once the countdown is over.
func (w *Wallet) VaultExecuteWithdraw(ctx context.Context) (string, error) {
	return w.vaultOwnerOp(ctx, opVaultExecuteWithdraw)
}

// SendClaims cashes vouchers, each to its own user's vault, MaxClaimsPerSend per transfer. Each
// claim carries VaultClaimGas; the vault returns the unused part with the payout, and a claim that
// fails (already paid, empty deposit) bounces it back. When the wallet cannot cover the gas for all
// of them it sends as many as it can, in the order given (put the biggest first). It returns the
// hashes of the transfers sent.
func (w *Wallet) SendClaims(ctx context.Context, list []SignedVoucher) ([]string, error) {
	bal, _, err := w.Balance(ctx)
	if err != nil {
		return nil, err
	}
	spare := new(big.Int).Sub(bal, FeeReserve)
	affordable := new(big.Int).Quo(spare, VaultClaimGas)
	if spare.Sign() <= 0 || affordable.Sign() <= 0 {
		return nil, fmt.Errorf("%w: collecting vouchers needs about %s TON per voucher for gas", ErrInsufficientFunds, FormatTON(VaultClaimGas))
	}
	if affordable.IsInt64() && affordable.Int64() < int64(len(list)) {
		list = list[:affordable.Int64()]
	}
	var hashes []string
	for start := 0; start < len(list); start += MaxClaimsPerSend {
		end := min(start+MaxClaimsPerSend, len(list))
		msgs := make([]*wallet.Message, 0, end-start)
		for i, sv := range list[start:end] {
			body, err := ClaimBody(sv, uint64(time.Now().Unix())<<16|uint64(i))
			if err != nil {
				return hashes, err
			}
			msgs = append(msgs, internalMsg(sv.Vault, VaultClaimGas, true, body, nil))
		}
		total := new(big.Int).Mul(VaultClaimGas, big.NewInt(int64(len(msgs))))
		h, err := w.send(ctx, msgs, total)
		if err != nil {
			return hashes, err
		}
		hashes = append(hashes, h)
	}
	return hashes, nil
}
