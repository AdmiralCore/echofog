// Package tonwallet is the self-custodial TON wallet built into EchoFog.
//
// The wallet key is the existing EchoFog wallet ed25519 key: TON uses the same curve, so the
// account address is derived offline and the private key never leaves the identity enclave.
// Signing goes through the TransactionSigner boundary; this package only builds messages,
// talks to TON liteservers and formats amounts.
package tonwallet

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// Network selects TON mainnet or testnet.
type Network string

const (
	Mainnet Network = "mainnet"
	Testnet Network = "testnet"
)

var (
	ErrFrozen            = errors.New("wallet transfers are frozen")
	ErrBadAddress        = errors.New("not a valid TON address")
	ErrBadAmount         = errors.New("amount must be a positive TON value with at most 9 decimals")
	ErrInsufficientFunds = errors.New("not enough TON on the balance (including the network fee)")
	ErrWrongNetwork      = errors.New("address belongs to the other TON network")
	ErrOffline           = errors.New("cannot reach the TON network")
)

// FeeReserve is kept aside for network fees when checking a transfer against the balance.
// A wallet v5 transfer costs ~0.003-0.006 TON, the first one also deploys the wallet.
var FeeReserve = big.NewInt(15_000_000) // 0.015 TON

// Signer is the identity security boundary (ProtectedIdentity satisfies it).
type Signer interface {
	SignWalletTransaction(data []byte) ([]byte, error)
	GetWalletPublicKey() ([]byte, error)
}

var configURLs = map[Network]string{
	Mainnet: "https://ton.org/global.config.json",
	Testnet: "https://ton.org/testnet-global.config.json",
}

// Wallet is a TON wallet v5r1 bound to the EchoFog wallet key.
type Wallet struct {
	net      Network
	pub      ed25519.PublicKey
	signer   Signer
	cacheDir string
	addr     *address.Address

	mu            sync.Mutex
	api           ton.APIClientWrapped
	w             *wallet.Wallet
	frozen        bool
	jettonWallets map[string]*address.Address // token symbol -> our jetton wallet
}

func versionConfig(net Network) wallet.ConfigV5R1Final {
	id := int32(wallet.MainnetGlobalID)
	if net == Testnet {
		id = wallet.TestnetGlobalID
	}
	return wallet.ConfigV5R1Final{NetworkGlobalID: id, Workchain: 0}
}

// New derives the wallet address offline; the network is contacted lazily on first use.
// cacheDir keeps the last liteserver config so the wallet still starts when ton.org is blocked.
func New(signer Signer, net Network, cacheDir string) (*Wallet, error) {
	if signer == nil {
		return nil, errors.New("signer is nil")
	}
	if net != Mainnet && net != Testnet {
		return nil, fmt.Errorf("unknown TON network %q", net)
	}
	pubBytes, err := signer.GetWalletPublicKey()
	if err != nil {
		return nil, fmt.Errorf("wallet public key: %w", err)
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid wallet public key size %d", len(pubBytes))
	}
	pub := ed25519.PublicKey(append([]byte(nil), pubBytes...))
	// Derive the address from the same wallet object that later signs, so the shown address and
	// the spending contract can never diverge (v5r1 uses subwallet 0, not the v3/v4 default).
	offline, err := wallet.FromPubKeyWithOptions(pub, versionConfig(net))
	if err != nil {
		return nil, fmt.Errorf("derive TON address: %w", err)
	}
	return &Wallet{net: net, pub: pub, signer: signer, cacheDir: cacheDir, addr: offline.WalletAddress()}, nil
}

// Network reports which TON network the wallet uses.
func (w *Wallet) Network() Network { return w.net }

// Address is the receive address in user-friendly non-bounceable form (UQ… / 0Q… on testnet),
// which is what wallets expect for an account that may not be deployed yet.
func (w *Wallet) Address() string {
	return w.addr.Bounce(false).Testnet(w.net == Testnet).String()
}

// RawAddress is the workchain:hex form used by explorers and indexers.
func (w *Wallet) RawAddress() string { return w.addr.StringRaw() }

// SetFrozen blocks outgoing transfers (panic switch / AI guardian).
func (w *Wallet) SetFrozen(frozen bool) {
	w.mu.Lock()
	w.frozen = frozen
	w.mu.Unlock()
}

func (w *Wallet) IsFrozen() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.frozen
}

// connect opens the liteserver pool once. Config: ton.org, falling back to the cached copy.
func (w *Wallet) connect(ctx context.Context) (ton.APIClientWrapped, *wallet.Wallet, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.api != nil {
		return w.api, w.w, nil
	}

	cfg, err := w.loadConfig(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	pool := liteclient.NewConnectionPool()
	if err := pool.AddConnectionsFromConfig(ctx, cfg); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	// The retry and timeout wrappers have newer replacements (WithRetryTimeout) with a per-attempt
	// timeout; this combination is kept until the new one has been checked against live liteservers.
	//lint:ignore SA1019 timeout semantics intentionally unchanged
	api := ton.NewAPIClient(pool, ton.ProofCheckPolicyFast).WithRetry(2).WithTimeout(15 * time.Second)

	sign := func(ctx context.Context, toSign *cell.Cell, _ uint32) ([]byte, error) {
		return w.signer.SignWalletTransaction(toSign.Hash())
	}
	tw, err := wallet.FromPubKeyWithOptions(w.pub, versionConfig(w.net), wallet.WithSigner(sign), wallet.WithAPI(api))
	if err != nil {
		return nil, nil, err
	}
	if tw.WalletAddress().StringRaw() != w.addr.StringRaw() {
		return nil, nil, errors.New("internal error: signing wallet address differs from the displayed address")
	}
	w.api, w.w = api, tw
	return api, tw, nil
}

func (w *Wallet) configCachePath() string {
	if w.cacheDir == "" {
		return ""
	}
	return filepath.Join(w.cacheDir, "ton-"+string(w.net)+"-global.config.json")
}

func (w *Wallet) loadConfig(ctx context.Context) (*liteclient.GlobalConfig, error) {
	raw, err := fetchConfig(ctx, configURLs[w.net])
	cachePath := w.configCachePath()
	if err == nil {
		cfg := &liteclient.GlobalConfig{}
		if err = json.Unmarshal(raw, cfg); err == nil {
			if cachePath != "" && os.MkdirAll(w.cacheDir, 0o700) == nil {
				_ = os.WriteFile(cachePath, raw, 0o600)
			}
			return cfg, nil
		}
	}
	if cachePath != "" {
		if cached, cerr := liteclient.GetConfigFromFile(cachePath); cerr == nil {
			return cached, nil
		}
	}
	return nil, err
}

func fetchConfig(ctx context.Context, u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("config download: HTTP %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 4<<20))
}

// Balance returns the confirmed balance in nanoTON and whether the wallet contract is deployed.
func (w *Wallet) Balance(ctx context.Context) (*big.Int, bool, error) {
	api, _, err := w.connect(ctx)
	if err != nil {
		return nil, false, err
	}
	block, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	acc, err := api.GetAccount(ctx, block, w.addr)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	if !acc.IsActive || acc.State == nil {
		return big.NewInt(0), false, nil
	}
	return acc.State.Balance.Nano(), acc.State.Status == tlb.AccountStatusActive, nil
}

// TransferRequest describes an outgoing payment.
type TransferRequest struct {
	To      string   // user-friendly or raw address
	Nano    *big.Int // amount in nanoTON
	Comment string   // plain-text comment (visible on chain)
}

// Transfer validates, checks the balance and broadcasts the payment. It returns the hash of the
// external message so the UI can follow its confirmation.
func (w *Wallet) Transfer(ctx context.Context, req TransferRequest) (string, error) {
	if w.IsFrozen() {
		return "", ErrFrozen
	}
	to, err := w.ParseRecipient(req.To)
	if err != nil {
		return "", err
	}
	if req.Nano == nil || req.Nano.Sign() <= 0 {
		return "", ErrBadAmount
	}
	if len(req.Comment) > 120 {
		return "", errors.New("comment is limited to 120 characters")
	}

	bal, _, err := w.Balance(ctx)
	if err != nil {
		return "", err
	}
	if new(big.Int).Add(req.Nano, FeeReserve).Cmp(bal) > 0 {
		return "", ErrInsufficientFunds
	}

	_, tw, err := w.connect(ctx)
	if err != nil {
		return "", err
	}
	// Bounce only to addresses that ask for it; payments to fresh wallets must not bounce back.
	msg, err := tw.BuildTransfer(to, tlb.FromNanoTON(req.Nano), to.IsBounceable(), req.Comment)
	if err != nil {
		return "", err
	}
	hash, err := tw.SendManyGetInMsgHash(ctx, []*wallet.Message{msg}, false)
	if err != nil {
		return "", fmt.Errorf("broadcast failed: %w", err)
	}
	return hex.EncodeToString(hash), nil
}

// ParseRecipient accepts friendly (EQ/UQ/kQ/0Q…) or raw (0:…) addresses and rejects ones that
// are explicitly marked for the other network.
func (w *Wallet) ParseRecipient(s string) (*address.Address, error) {
	s = strings.TrimSpace(s)
	var (
		a   *address.Address
		err error
	)
	if strings.Contains(s, ":") {
		a, err = address.ParseRawAddr(s)
	} else {
		a, err = address.ParseAddr(s)
	}
	if err != nil || a == nil {
		return nil, ErrBadAddress
	}
	if a.IsTestnetOnly() && w.net == Mainnet {
		return nil, ErrWrongNetwork
	}
	return a, nil
}

// Tx is one entry of the wallet history.
type Tx struct {
	Asset        string    `json:"asset"` // "TON" or a token symbol such as "USDT"
	Hash         string    `json:"hash"`
	LT           uint64    `json:"lt"`
	Time         time.Time `json:"time"`
	Incoming     bool      `json:"incoming"`
	AmountNano   string    `json:"amountNano"`
	Counterparty string    `json:"counterparty"`
	Comment      string    `json:"comment"`
	FeeNano      string    `json:"feeNano"`
	Failed       bool      `json:"failed"`
}

// History returns the latest transactions, newest first.
func (w *Wallet) History(ctx context.Context, limit uint32) ([]Tx, error) {
	if limit == 0 || limit > 50 {
		limit = 20
	}
	api, _, err := w.connect(ctx)
	if err != nil {
		return nil, err
	}
	block, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	acc, err := api.GetAccount(ctx, block, w.addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	if acc.LastTxLT == 0 {
		return []Tx{}, nil
	}
	list, err := api.ListTransactions(ctx, w.addr, limit, acc.LastTxLT, acc.LastTxHash)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	usdtWallet := ""
	if _, err := w.tokenMaster(USDT); err == nil {
		if jw, err := w.TokenWalletAddress(ctx, USDT); err == nil {
			usdtWallet = jw.StringRaw()
		}
	}
	out := make([]Tx, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- { // ListTransactions is oldest first
		out = append(out, toTx(list[i], w.net, usdtWallet))
	}
	return out, nil
}

func toTx(t *tlb.Transaction, net Network, usdtWallet string) Tx {
	tx := Tx{
		Asset:   "TON",
		Hash:    hex.EncodeToString(t.Hash),
		LT:      t.LT,
		Time:    time.Unix(int64(t.Now), 0),
		FeeNano: t.TotalFees.Coins.Nano().String(),
	}
	friendly := func(a *address.Address) string {
		if a == nil {
			return ""
		}
		return a.Testnet(net == Testnet).String()
	}
	if amount, incoming, who, comment, ok := tokenFromTx(t, usdtWallet, friendly); ok {
		tx.Asset, tx.AmountNano, tx.Incoming, tx.Counterparty, tx.Comment = USDT.Symbol, amount.String(), incoming, who, comment
		if desc, ok := t.Description.(tlb.TransactionDescriptionOrdinary); ok {
			tx.Failed = desc.Aborted
		}
		return tx
	}
	if in := t.IO.In; in != nil && in.MsgType == tlb.MsgTypeInternal {
		im := in.AsInternal()
		if im.Amount.Nano().Sign() > 0 {
			tx.Incoming = true
			tx.AmountNano = im.Amount.Nano().String()
			tx.Counterparty = friendly(im.SrcAddr)
			tx.Comment = im.Comment()
			return tx
		}
	}
	// outgoing: sum internal messages sent by this transaction
	total := new(big.Int)
	if t.IO.Out != nil {
		if msgs, err := t.IO.Out.ToSlice(); err == nil {
			for _, m := range msgs {
				if m.MsgType != tlb.MsgTypeInternal {
					continue
				}
				im := m.AsInternal()
				total.Add(total, im.Amount.Nano())
				if tx.Counterparty == "" {
					tx.Counterparty = friendly(im.DstAddr)
					tx.Comment = im.Comment()
				}
			}
		}
	}
	tx.AmountNano = total.String()
	if desc, ok := t.Description.(tlb.TransactionDescriptionOrdinary); ok {
		tx.Failed = desc.Aborted
	}
	return tx
}

// TopUpLink is the ton:// deep link encoded in the receive QR code. Telegram Wallet, Tonkeeper
// and MyTonWallet open it with the address, amount and comment pre-filled.
func TopUpLink(addr string, nano *big.Int, comment string) string {
	q := url.Values{}
	if nano != nil && nano.Sign() > 0 {
		q.Set("amount", nano.String())
	}
	if comment != "" {
		q.Set("text", comment)
	}
	link := "ton://transfer/" + addr
	if enc := q.Encode(); enc != "" {
		link += "?" + enc
	}
	return link
}

// ParseTON converts a decimal TON string ("0.5", "12", "0,25") into nanoTON exactly.
func ParseTON(s string) (*big.Int, error) { return ParseUnits(s, 9) }

// FormatTON renders nanoTON as a decimal TON string without trailing zeros.
func FormatTON(nano *big.Int) string { return FormatUnits(nano, 9) }
