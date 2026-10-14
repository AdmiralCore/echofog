package tonwallet

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton/jetton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// Token is a jetton the wallet shows and sends.
type Token struct {
	Symbol        string
	Decimals      int
	MasterMainnet string
	MasterTestnet string // empty: not available on testnet
}

// USDT is Tether's official USD₮ jetton on TON.
var USDT = Token{
	Symbol:        "USDT",
	Decimals:      6,
	MasterMainnet: "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs",
}

const ()

var (
	ErrTokenUnavailable = errors.New("this token is not available on the selected TON network")
	ErrNeedTONForFee    = errors.New("token transfers need a little TON for the network fee (about 0.06 TON)")
)

// JettonGas is attached to a jetton transfer; the jetton wallet returns what is not spent.
var JettonGas = big.NewInt(50_000_000) // 0.05 TON

// forwardNotify makes the recipient's wallet show the transfer and its comment.
var forwardNotify = big.NewInt(1)

func (w *Wallet) tokenMaster(t Token) (*address.Address, error) {
	raw := t.MasterMainnet
	if w.net == Testnet {
		raw = t.MasterTestnet
	}
	if raw == "" {
		return nil, ErrTokenUnavailable
	}
	return address.ParseAddr(raw)
}

// TokenWalletAddress resolves (and caches) our jetton wallet for t via the master contract.
// Only messages from this exact address are trusted as incoming transfers of t.
func (w *Wallet) TokenWalletAddress(ctx context.Context, t Token) (*address.Address, error) {
	w.mu.Lock()
	if a, ok := w.jettonWallets[t.Symbol]; ok {
		w.mu.Unlock()
		return a, nil
	}
	w.mu.Unlock()

	api, _, err := w.connect(ctx)
	if err != nil {
		return nil, err
	}
	master, err := w.tokenMaster(t)
	if err != nil {
		return nil, err
	}
	jw, err := jetton.NewJettonMasterClient(api, master).GetJettonWallet(ctx, w.addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	w.mu.Lock()
	if w.jettonWallets == nil {
		w.jettonWallets = make(map[string]*address.Address)
	}
	w.jettonWallets[t.Symbol] = jw.Address()
	w.mu.Unlock()
	return jw.Address(), nil
}

// TokenBalance returns the balance of t in its smallest units (USDT: 1e-6).
func (w *Wallet) TokenBalance(ctx context.Context, t Token) (*big.Int, error) {
	api, _, err := w.connect(ctx)
	if err != nil {
		return nil, err
	}
	jwAddr, err := w.TokenWalletAddress(ctx, t)
	if err != nil {
		return nil, err
	}
	block, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	acc, err := api.GetAccount(ctx, block, jwAddr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	if !acc.IsActive || acc.State == nil || acc.State.Status != tlb.AccountStatusActive {
		return big.NewInt(0), nil // never received this token
	}
	return w.tokenBalanceOf(ctx, t, w.addr)
}

// tokenBalanceOf reads the jetton balance of any owner (used by tests against real holders).
func (w *Wallet) tokenBalanceOf(ctx context.Context, t Token, owner *address.Address) (*big.Int, error) {
	api, _, err := w.connect(ctx)
	if err != nil {
		return nil, err
	}
	master, err := w.tokenMaster(t)
	if err != nil {
		return nil, err
	}
	block, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	jw, err := jetton.NewJettonMasterClient(api, master).GetJettonWalletAtBlock(ctx, owner, block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	bal, err := jw.GetBalanceAtBlock(ctx, block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	return bal, nil
}

// buildTokenTransfer builds the internal message to our jetton wallet. Excess gas returns to us.
func buildTokenTransfer(jwAddr, to, owner *address.Address, units *big.Int, comment string) (*wallet.Message, error) {
	var fwd *cell.Cell
	if comment != "" {
		c, err := wallet.CreateCommentCell(comment)
		if err != nil {
			return nil, err
		}
		fwd = c
	}
	body, err := jetton.BuildTransferPayload(to, owner, tlb.FromNanoTON(units), tlb.FromNanoTON(forwardNotify), fwd, nil)
	if err != nil {
		return nil, err
	}
	// bounce: if the jetton wallet rejects the transfer, the attached TON comes back
	return wallet.SimpleMessage(jwAddr, tlb.FromNanoTON(JettonGas), body), nil
}

// TokenTransfer sends units of t to another TON address and returns the external message hash.
func (w *Wallet) TokenTransfer(ctx context.Context, t Token, to string, units *big.Int, comment string) (string, error) {
	if w.IsFrozen() {
		return "", ErrFrozen
	}
	dst, err := w.ParseRecipient(to)
	if err != nil {
		return "", err
	}
	if units == nil || units.Sign() <= 0 {
		return "", ErrBadAmount
	}
	if len(comment) > 120 {
		return "", errors.New("comment is limited to 120 characters")
	}
	have, err := w.TokenBalance(ctx, t)
	if err != nil {
		return "", err
	}
	if units.Cmp(have) > 0 {
		return "", fmt.Errorf("not enough %s on the balance", t.Symbol)
	}
	tonBal, _, err := w.Balance(ctx)
	if err != nil {
		return "", err
	}
	if new(big.Int).Add(JettonGas, FeeReserve).Cmp(tonBal) > 0 {
		return "", ErrNeedTONForFee
	}
	jwAddr, err := w.TokenWalletAddress(ctx, t)
	if err != nil {
		return "", err
	}
	msg, err := buildTokenTransfer(jwAddr, dst, w.addr, units, comment)
	if err != nil {
		return "", err
	}
	_, tw, err := w.connect(ctx)
	if err != nil {
		return "", err
	}
	hash, err := tw.SendManyGetInMsgHash(ctx, []*wallet.Message{msg}, false)
	if err != nil {
		return "", fmt.Errorf("broadcast failed: %w", err)
	}
	return hex.EncodeToString(hash), nil
}

// ParseUnits converts a decimal string into integer units with the given number of decimals.
func ParseUnits(s string, decimals int) (*big.Int, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", ".")
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return nil, ErrBadAmount
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > decimals || strings.Contains(frac, ".") {
		return nil, ErrBadAmount
	}
	frac += strings.Repeat("0", decimals-len(frac))
	n, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok || n.Sign() <= 0 {
		return nil, ErrBadAmount
	}
	return n, nil
}

// FormatUnits renders integer units as a decimal string without trailing zeros.
func FormatUnits(v *big.Int, decimals int) string {
	if v == nil {
		return "0"
	}
	neg := v.Sign() < 0
	abs := new(big.Int).Abs(v)
	q, r := new(big.Int).QuoRem(abs, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil), new(big.Int))
	out := q.String()
	if r.Sign() != 0 {
		frac := fmt.Sprintf("%0*s", decimals, r.String())
		out += "." + strings.TrimRight(frac, "0")
	}
	if neg {
		out = "-" + out
	}
	return out
}

// tokenFromTx recognises jetton movements in a wallet transaction: an incoming transfer
// notification from our own jetton wallet, or our outgoing transfer request to it.
func tokenFromTx(t *tlb.Transaction, jwRaw string, friendly func(*address.Address) string) (amount *big.Int, incoming bool, counterparty, comment string, ok bool) {
	if jwRaw == "" {
		return nil, false, "", "", false
	}
	if in := t.IO.In; in != nil && in.MsgType == tlb.MsgTypeInternal {
		im := in.AsInternal()
		// a notification is only trusted from the jetton wallet the master assigned to us;
		// anyone can deploy a fake token that sends look-alike notifications
		if im.SrcAddr != nil && im.SrcAddr.StringRaw() == jwRaw && im.Body != nil {
			var n jetton.TransferNotification
			if err := loadCell(&n, im.Body); err == nil {
				return n.Amount.Nano(), true, friendly(n.Sender), commentOf(n.ForwardPayload), true
			}
		}
	}
	if t.IO.Out == nil {
		return nil, false, "", "", false
	}
	msgs, err := t.IO.Out.ToSlice()
	if err != nil {
		return nil, false, "", "", false
	}
	for _, m := range msgs {
		if m.MsgType != tlb.MsgTypeInternal {
			continue
		}
		im := m.AsInternal()
		if im.DstAddr == nil || im.DstAddr.StringRaw() != jwRaw || im.Body == nil {
			continue
		}
		var p jetton.TransferPayload
		if err := loadCell(&p, im.Body); err == nil {
			return p.Amount.Nano(), false, friendly(p.Destination), commentOf(p.ForwardPayload), true
		}
	}
	return nil, false, "", "", false
}

func commentOf(c *cell.Cell) string {
	if c == nil {
		return ""
	}
	s, err := c.BeginParse()
	if err != nil {
		return ""
	}
	op, err := s.LoadUInt(32)
	if err != nil || op != 0 {
		return ""
	}
	text, err := s.LoadStringSnake()
	if err != nil {
		return ""
	}
	return text
}

func loadCell(dst any, c *cell.Cell) error {
	s, err := c.BeginParse()
	if err != nil {
		return err
	}
	return tlb.LoadFromCell(dst, s)
}
