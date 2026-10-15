package exit_gateway

// Paying an exit with vouchers from the client's EchoFogVault (see contracts/vault).
//
// The client keeps TON in its own vault contract and, before it uses the next chunk of traffic,
// signs a voucher: "this exit may take X nanoton from my vault, in total". The exit checks the
// signature and, now and then, that the vault really holds the money and is not being withdrawn,
// then credits the chunk. Nothing goes on-chain per chunk. Every few hours, or once enough has
// piled up, the exit cashes the latest voucher of each client in one batch.
//
// What either side risks: the client, one chunk paid ahead and not served. The exit, what it has
// not cashed yet from a client who also signed away the same deposit to other exits; it keeps
// that small by cashing a client early once it owes more than ClaimPolicy.UrgentDelta.

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/xssnick/tonutils-go/address"
)

const voucherOfferVersion = 1

const (
	// vaultCheckEvery: a vault is read from the chain again before a voucher is accepted if the
	// last read is older than this. Withdrawals take 24 h, so a minute-old view is safe.
	vaultCheckEvery = time.Minute
	// vaultStaleOK: while the chain cannot be reached, a view up to this old still lets the exit
	// accept new vouchers from a vault it knows.
	vaultStaleOK = 15 * time.Minute
	// claimSettle: after sending claims, wait this long before reading the result from the chain.
	claimSettle = 2 * time.Minute
	// voucherKeepIdle: an account with nothing owed and nothing left is forgotten after this.
	voucherKeepIdle = 30 * 24 * time.Hour
	voucherFlushEvery = 5 * time.Second
)

var (
	ErrVoucherRejected = errors.New("voucher rejected")
	ErrVaultUnusable   = errors.New("the vault cannot pay")
)

// VoucherOffer is what a client sends to buy traffic: its vault (as owner and key) and a signed
// cumulative amount for this exit.
type VoucherOffer struct {
	Version    int    `json:"v"`
	Owner      string `json:"owner"`      // vault owner, raw address
	PubKey     string `json:"pubkey"`     // hex ed25519 key that signs
	Cumulative string `json:"cumulative"` // nanoton, decimal
	Sig        string `json:"sig"`        // hex signature of the voucher cell hash
}

// VoucherReply is the exit's answer to an offer.
type VoucherReply struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	Credit     int64  `json:"credit"`     // voucher-paid bytes left
	Cumulative string `json:"cumulative"` // latest voucher the exit holds for this vault
}

// VaultTerms is the voucher part of AccountInfo: how to pay this exit from a vault.
type VaultTerms struct {
	NanoPerMB int64  `json:"nano_per_mb"` // the exit's price in nanoton, from its USD price
	Node      string `json:"node"`        // payout address the voucher must name (raw)
	GlobalID  int32  `json:"global_id"`
	Credit    int64  `json:"credit"` // voucher-paid bytes left for the caller
	// The latest voucher the exit holds from the caller's vault, so a client that lost its own
	// record continues from there. The client checks the signature: an exit cannot make it sign
	// on top of an amount it never signed.
	Cumulative string `json:"cumulative,omitempty"`
	Sig        string `json:"sig,omitempty"`
}

// VaultChain is what the voucher book needs from TON: the exit's own wallet, or a fake in tests.
type VaultChain interface {
	ReadVault(ctx context.Context, vault *address.Address) (tonwallet.VaultInfo, error)
	VaultClaimed(ctx context.Context, vault, node *address.Address) (*big.Int, error)
	SendClaims(ctx context.Context, list []tonwallet.SignedVoucher) ([]string, error)
}

// ClaimPolicy decides when vouchers are worth cashing. A claim costs the exit about 0.0012 TON
// plus its share of the wallet fee, whatever the amount, so small ones wait: nothing is lost by
// waiting, because a later voucher includes everything before it.
type ClaimPolicy struct {
	MinDelta        *big.Int      // a client's voucher is cashed only once it owes at least this
	UrgentDelta     *big.Int      // ... and at once when it owes this much (limits what a cheat can take)
	BatchTotal      *big.Int      // cash everything eligible as soon as it adds up to this
	Every           time.Duration // otherwise once per this period
	ClosingMinDelta *big.Int      // a vault being withdrawn is cashed down to this, before it empties
	Tick            time.Duration
	RefreshEvery    time.Duration // how often vaults that owe something are re-read from the chain
}

// DefaultClaimPolicy: 0.02 TON per client is about 6% gas, 0.05 TON triggers an early claim.
func DefaultClaimPolicy() ClaimPolicy {
	return ClaimPolicy{
		MinDelta:        big.NewInt(20_000_000),
		UrgentDelta:     big.NewInt(50_000_000),
		BatchTotal:      big.NewInt(200_000_000),
		Every:           6 * time.Hour,
		ClosingMinDelta: big.NewInt(3_000_000),
		Tick:            5 * time.Minute,
		RefreshEvery:    time.Hour,
	}
}

type voucherAccount struct {
	Owner      string `json:"owner"`
	PubKey     string `json:"pubkey"`
	Cumulative string `json:"cumulative"` // latest accepted voucher, nanoton
	Sig        string `json:"sig"`
	Credit     int64  `json:"credit"` // bytes bought and not used yet
	Claimed    string `json:"claimed"` // paid to us on-chain, as last read
	Deposit    string `json:"deposit"` // as last read
	WithdrawAt int64  `json:"withdraw_at,omitempty"`
	CheckedAt  int64  `json:"checked_at,omitempty"`
	ClaimedAt  int64  `json:"claim_sent_at,omitempty"`
	Updated    int64  `json:"updated"`
}

type voucherFile struct {
	Accounts  map[string]*voucherAccount `json:"accounts"` // vault raw address -> account
	Peers     map[string]string          `json:"peers"`    // pay tag -> vault raw address
	LastClaim int64                      `json:"last_claim,omitempty"`
}

// VoucherBook is the exit's record of what each vault has signed, what of it was used and what
// was cashed. It survives restarts: losing it would lose unclaimed money.
type VoucherBook struct {
	globalID int32
	node     *address.Address // our payout address
	chain    VaultChain

	mu        sync.Mutex
	path      string
	data      voucherFile
	dirty     bool
	nanoPerMB int64
	now       func() time.Time
	checking  map[string]bool // vaults being read from the chain right now
}

// NewVoucherBook loads the book from path ("" keeps it in memory). node is the exit's payout
// address, the one vouchers must name.
func NewVoucherBook(path string, globalID int32, node *address.Address, chain VaultChain) *VoucherBook {
	b := &VoucherBook{globalID: globalID, node: node, chain: chain, path: path, now: time.Now, checking: map[string]bool{}}
	b.data.Accounts = map[string]*voucherAccount{}
	b.data.Peers = map[string]string{}
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			var f voucherFile
			if json.Unmarshal(raw, &f) == nil {
				if f.Accounts != nil {
					b.data.Accounts = f.Accounts
				}
				if f.Peers != nil {
					b.data.Peers = f.Peers
				}
				b.data.LastClaim = f.LastClaim
			}
		}
	}
	return b
}

// SetQuote sets the price in nanoton per MB that new vouchers are credited at. 0 stops taking
// vouchers (for example while the TON rate is unknown).
func (b *VoucherBook) SetQuote(nanoPerMB int64) {
	b.mu.Lock()
	b.nanoPerMB = nanoPerMB
	b.mu.Unlock()
}

// Quote is the current nanoton price per MB.
func (b *VoucherBook) Quote() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nanoPerMB
}

func bigOf(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok || n.Sign() < 0 {
		return new(big.Int)
	}
	return n
}

// Terms is what AccountInfo tells the client with pay tag tag; nil when no quote is set.
func (b *VoucherBook) Terms(tag string) *VaultTerms {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nanoPerMB <= 0 || b.node == nil {
		return nil
	}
	t := &VaultTerms{NanoPerMB: b.nanoPerMB, Node: b.node.StringRaw(), GlobalID: b.globalID}
	if a := b.data.Accounts[b.data.Peers[tag]]; a != nil {
		t.Credit, t.Cumulative, t.Sig = a.Credit, a.Cumulative, a.Sig
	}
	return t
}

// Available is the voucher-paid bytes tag can still use.
func (b *VoucherBook) Available(tag string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if a := b.data.Accounts[b.data.Peers[tag]]; a != nil && a.Credit > 0 {
		return a.Credit
	}
	return 0
}

// Spend uses up to n bytes of tag's voucher credit and returns how many it covered.
func (b *VoucherBook) Spend(tag string, n int64) int64 {
	if n <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.data.Accounts[b.data.Peers[tag]]
	if a == nil || a.Credit <= 0 {
		return 0
	}
	used := min(n, a.Credit)
	a.Credit -= used
	b.dirty = true
	return used
}

// parseOffer turns an offer into the voucher it claims to sign, for this exit and network.
func (b *VoucherBook) parseOffer(o VoucherOffer) (tonwallet.SignedVoucher, ed25519.PublicKey, *address.Address, error) {
	var zero tonwallet.SignedVoucher
	if o.Version != voucherOfferVersion {
		return zero, nil, nil, fmt.Errorf("%w: unknown version", ErrVoucherRejected)
	}
	owner, err := address.ParseRawAddr(o.Owner)
	if err != nil {
		return zero, nil, nil, fmt.Errorf("%w: bad owner", ErrVoucherRejected)
	}
	pub, err := hex.DecodeString(o.PubKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return zero, nil, nil, fmt.Errorf("%w: bad key", ErrVoucherRejected)
	}
	sig, err := hex.DecodeString(o.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return zero, nil, nil, fmt.Errorf("%w: bad signature", ErrVoucherRejected)
	}
	cum, ok := new(big.Int).SetString(o.Cumulative, 10)
	if !ok || cum.Sign() <= 0 || cum.BitLen() > 120 {
		return zero, nil, nil, fmt.Errorf("%w: bad amount", ErrVoucherRejected)
	}
	// The vault address is derived here from the real contract code, never taken from the client.
	vault, err := tonwallet.VaultAddress(owner, pub)
	if err != nil {
		return zero, nil, nil, fmt.Errorf("%w: %v", ErrVoucherRejected, err)
	}
	v := tonwallet.Voucher{GlobalID: b.globalID, Vault: vault, Node: b.node, Cumulative: cum}
	if !tonwallet.VerifyVoucher(pub, v, sig) {
		return zero, nil, nil, fmt.Errorf("%w: signature does not match", ErrVoucherRejected)
	}
	return tonwallet.SignedVoucher{Voucher: v, Signature: sig}, pub, owner, nil
}

// refresh reads vault's deposit, withdrawal and what it paid us. It never holds the lock while
// waiting for the chain.
func (b *VoucherBook) refresh(ctx context.Context, vault *address.Address) error {
	key := vault.StringRaw()
	b.mu.Lock()
	if b.checking[key] {
		b.mu.Unlock()
		return nil // someone else is reading it right now
	}
	b.checking[key] = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.checking, key)
		b.mu.Unlock()
	}()

	info, err := b.chain.ReadVault(ctx, vault)
	if err != nil {
		return err
	}
	claimed := new(big.Int)
	if info.Deployed {
		if claimed, err = b.chain.VaultClaimed(ctx, vault, b.node); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.data.Accounts[key]
	if a == nil {
		a = &voucherAccount{Cumulative: "0", Claimed: "0", Deposit: "0"}
		b.data.Accounts[key] = a
	}
	if info.Deposit == nil {
		info.Deposit = new(big.Int)
	}
	a.Deposit = info.Deposit.String()
	a.Claimed = claimed.String()
	a.WithdrawAt = 0
	if info.Closing() {
		a.WithdrawAt = info.WithdrawAt.Unix()
	}
	if !info.Deployed {
		a.Deposit = "0"
	}
	a.CheckedAt = b.now().Unix()
	b.dirty = true
	return nil
}

// Accept checks a client's voucher and credits the traffic it newly pays for. A voucher that is
// not newer than the one held is accepted again without crediting anything, so a client that
// repeats itself after a lost reply is not charged twice.
func (b *VoucherBook) Accept(ctx context.Context, tag string, o VoucherOffer) VoucherReply {
	sv, pub, owner, err := b.parseOffer(o)
	if err != nil {
		return VoucherReply{Error: err.Error()}
	}
	key := sv.Vault.StringRaw()

	b.mu.Lock()
	price := b.nanoPerMB
	a := b.data.Accounts[key]
	var checked time.Time
	if a != nil {
		checked = time.Unix(a.CheckedAt, 0)
	}
	now := b.now()
	b.mu.Unlock()
	if price <= 0 {
		return VoucherReply{Error: "this exit does not take vouchers right now"}
	}
	if now.Sub(checked) > vaultCheckEvery {
		if err := b.refresh(ctx, sv.Vault); err != nil && now.Sub(checked) > vaultStaleOK {
			return VoucherReply{Error: "cannot check the vault on the TON network, try again shortly"}
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	a = b.data.Accounts[key]
	if a == nil || a.CheckedAt == 0 {
		return VoucherReply{Error: "cannot check the vault on the TON network, try again shortly"}
	}
	b.data.Peers[tag] = key
	prev := bigOf(a.Cumulative)
	reply := func() VoucherReply { return VoucherReply{OK: true, Credit: a.Credit, Cumulative: a.Cumulative} }
	if sv.Cumulative.Cmp(prev) <= 0 {
		return reply()
	}
	deposit := bigOf(a.Deposit)
	switch {
	case deposit.Sign() == 0:
		return VoucherReply{Error: fmt.Sprintf("%v: the vault is empty", ErrVaultUnusable), Credit: a.Credit, Cumulative: a.Cumulative}
	case a.WithdrawAt != 0:
		return VoucherReply{Error: fmt.Sprintf("%v: it is being withdrawn", ErrVaultUnusable), Credit: a.Credit, Cumulative: a.Cumulative}
	}
	// what this voucher would let us take must still be in the vault
	owed := new(big.Int).Sub(sv.Cumulative, bigOf(a.Claimed))
	if owed.Cmp(deposit) > 0 {
		return VoucherReply{Error: fmt.Sprintf("%v: the deposit is too small for this voucher, top it up", ErrVaultUnusable), Credit: a.Credit, Cumulative: a.Cumulative}
	}

	delta := new(big.Int).Sub(sv.Cumulative, prev)
	bought := new(big.Int).Mul(delta, big.NewInt(bytesPerMB))
	bought.Quo(bought, big.NewInt(price))
	if !bought.IsInt64() {
		return VoucherReply{Error: fmt.Sprintf("%v: amount too large", ErrVoucherRejected)}
	}
	a.Owner, a.PubKey = owner.StringRaw(), hex.EncodeToString(pub)
	a.Cumulative, a.Sig = sv.Cumulative.String(), hex.EncodeToString(sv.Signature)
	a.Credit += bought.Int64()
	a.Updated = now.Unix()
	b.dirty = true
	_ = b.flushLocked() // a signed voucher is money: never leave it to the timer
	return reply()
}

// HandleVoucherFrom answers a TypeExitVoucher request: the first frame holds the offer.
func (e *ExitService) HandleVoucherFrom(ctx context.Context, st FramedStream, payload []byte, peer types.PeerID) {
	defer st.Close()
	e.policyMu.Lock()
	book, self, free := e.vouchers, e.self, e.freeLocked(peer)
	e.policyMu.Unlock()
	reply := VoucherReply{Error: "this exit does not take vouchers"}
	var o VoucherOffer
	switch {
	case free:
		reply.Error = "this exit is free for you"
	case book == nil || !e.enabled.Load():
	case json.Unmarshal(payload, &o) != nil:
		reply.Error = "malformed offer"
	default:
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		reply = book.Accept(cctx, PayTag(peer, self), o)
		cancel()
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return
	}
	_ = st.WriteFrame(net_types.TypeExitVoucher, raw)
}

// SetVouchers lets this exit take vouchers; book nil stops it.
func (e *ExitService) SetVouchers(book *VoucherBook) {
	e.policyMu.Lock()
	e.vouchers = book
	e.policyMu.Unlock()
}

// SendVoucher offers a voucher to the exit and returns its answer.
func (d *P2PDialer) SendVoucher(ctx context.Context, o VoucherOffer) (*VoucherReply, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	b, err := d.roundTrip(ctx, net_types.TypeExitVoucher, raw)
	if err != nil {
		return nil, err
	}
	var r VoucherReply
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, ErrExitProtocol
	}
	return &r, nil
}

// roundTrip sends one frame on a new stream and waits (at most 20 s) for one reply of the same type.
func (d *P2PDialer) roundTrip(ctx context.Context, t net_types.MessageType, payload []byte) ([]byte, error) {
	if d == nil || d.open == nil {
		return nil, errNoStreamOpener
	}
	st, err := d.open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open stream to exit peer: %w", err)
	}
	defer st.Close()
	if err := st.WriteFrame(t, payload); err != nil {
		return nil, err
	}
	type reply struct {
		t   net_types.MessageType
		b   []byte
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		rt, b, err := st.ReadFrame()
		ch <- reply{rt, b, err}
	}()
	timeout := time.NewTimer(20 * time.Second)
	defer timeout.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		if r.t != t {
			return nil, ErrExitProtocol
		}
		return r.b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout.C:
		return nil, context.DeadlineExceeded
	}
}

// ---- cashing vouchers ----

// claimable picks the vouchers worth cashing now under p, and reports whether to send them now.
func (b *VoucherBook) claimable(p ClaimPolicy, now time.Time) ([]tonwallet.SignedVoucher, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	type cand struct {
		sv    tonwallet.SignedVoucher
		owed  *big.Int
		force bool
	}
	var list []cand
	total := new(big.Int)
	urgent := false
	for key, a := range b.data.Accounts {
		if a.Sig == "" || (a.ClaimedAt != 0 && now.Sub(time.Unix(a.ClaimedAt, 0)) < claimSettle+p.Tick) {
			continue // nothing signed, or a claim is still on its way
		}
		owed := new(big.Int).Sub(bigOf(a.Cumulative), bigOf(a.Claimed))
		if owed.Sign() <= 0 || bigOf(a.Deposit).Sign() == 0 {
			continue
		}
		closing := a.WithdrawAt != 0
		floor := p.MinDelta
		if closing {
			floor = p.ClosingMinDelta
		}
		if owed.Cmp(floor) < 0 {
			continue
		}
		vault, err := address.ParseRawAddr(key)
		if err != nil {
			continue
		}
		sig, err := hex.DecodeString(a.Sig)
		if err != nil {
			continue
		}
		sv := tonwallet.SignedVoucher{
			Voucher:   tonwallet.Voucher{GlobalID: b.globalID, Vault: vault.Bounce(true), Node: b.node, Cumulative: bigOf(a.Cumulative)},
			Signature: sig,
		}
		force := closing || owed.Cmp(p.UrgentDelta) >= 0
		urgent = urgent || force
		total.Add(total, owed)
		list = append(list, cand{sv, owed, force})
	}
	if len(list) == 0 {
		return nil, false
	}
	due := urgent || total.Cmp(p.BatchTotal) >= 0 || now.Sub(time.Unix(b.data.LastClaim, 0)) >= p.Every
	// biggest first, so a batch cut short by the wallet balance cashes what matters most
	sort.Slice(list, func(i, j int) bool { return list[i].owed.Cmp(list[j].owed) > 0 })
	out := make([]tonwallet.SignedVoucher, len(list))
	for i, c := range list {
		out[i] = c.sv
	}
	return out, due
}

// ClaimOnce runs one round: re-reads vaults that owe us and are due a check, then cashes what the
// policy says is worth it. It returns how many vouchers it sent.
func (b *VoucherBook) ClaimOnce(ctx context.Context, p ClaimPolicy) (int, error) {
	now := b.now()
	b.mu.Lock()
	var stale []string
	for key, a := range b.data.Accounts {
		owed := new(big.Int).Sub(bigOf(a.Cumulative), bigOf(a.Claimed))
		sent := a.ClaimedAt != 0 && now.Sub(time.Unix(a.ClaimedAt, 0)) >= claimSettle
		if owed.Sign() > 0 && (sent || now.Sub(time.Unix(a.CheckedAt, 0)) >= p.RefreshEvery) {
			stale = append(stale, key)
		}
	}
	b.mu.Unlock()
	for _, key := range stale {
		vault, err := address.ParseRawAddr(key)
		if err != nil {
			continue
		}
		if err := b.refresh(ctx, vault.Bounce(true)); err != nil {
			slog.Debug("vault check failed", "vault", key, "err", err)
			continue
		}
		b.mu.Lock()
		if a := b.data.Accounts[key]; a != nil && a.ClaimedAt != 0 && now.Sub(time.Unix(a.ClaimedAt, 0)) >= claimSettle {
			a.ClaimedAt = 0 // the result is in: the next round may claim it again if still owed
		}
		b.mu.Unlock()
	}

	list, due := b.claimable(p, now)
	if !due || len(list) == 0 {
		b.prune(now)
		return 0, nil
	}
	if _, err := b.chain.SendClaims(ctx, list); err != nil {
		return 0, err
	}
	b.mu.Lock()
	for _, sv := range list {
		if a := b.data.Accounts[sv.Vault.StringRaw()]; a != nil {
			a.ClaimedAt = now.Unix()
		}
	}
	b.data.LastClaim = now.Unix()
	b.dirty = true
	_ = b.flushLocked()
	b.mu.Unlock()
	slog.Info("cashed vouchers", "count", len(list))
	return len(list), nil
}

// RunClaims cashes vouchers in the background until ctx ends.
func (b *VoucherBook) RunClaims(ctx context.Context, p ClaimPolicy) {
	t := time.NewTicker(p.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n, err := b.ClaimOnce(ctx, p); err != nil {
			slog.Warn("could not cash vouchers, will retry", "err", err)
		} else if n > 0 {
			slog.Debug("voucher claims sent", "count", n)
		}
	}
}

// Owed is the total signed to us and not cashed yet, in nanoton.
func (b *VoucherBook) Owed() *big.Int {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := new(big.Int)
	for _, a := range b.data.Accounts {
		if d := new(big.Int).Sub(bigOf(a.Cumulative), bigOf(a.Claimed)); d.Sign() > 0 {
			total.Add(total, d)
		}
	}
	return total
}

// Cashed is the total paid to us from all vaults, as last read.
func (b *VoucherBook) Cashed() *big.Int {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := new(big.Int)
	for _, a := range b.data.Accounts {
		total.Add(total, bigOf(a.Claimed))
	}
	return total
}

// prune forgets accounts that owe nothing, hold no credit and have been idle for a month.
func (b *VoucherBook) prune(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cut := now.Add(-voucherKeepIdle).Unix()
	for key, a := range b.data.Accounts {
		owed := new(big.Int).Sub(bigOf(a.Cumulative), bigOf(a.Claimed))
		if owed.Sign() <= 0 && a.Credit <= 0 && a.Updated < cut && a.CheckedAt < cut {
			delete(b.data.Accounts, key)
			b.dirty = true
		}
	}
	for tag, key := range b.data.Peers {
		if b.data.Accounts[key] == nil {
			delete(b.data.Peers, tag)
			b.dirty = true
		}
	}
}

// ---- persistence ----

// Start saves credit use in the background, so a per-packet update never touches disk.
func (b *VoucherBook) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(voucherFlushEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = b.Flush()
			case <-ctx.Done():
				_ = b.Flush()
				return
			}
		}
	}()
}

// Flush writes the book if it changed.
func (b *VoucherBook) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flushLocked()
}

func (b *VoucherBook) flushLocked() error {
	if b.path == "" || !b.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(&b.data)
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return err
	}
	b.dirty = false
	return nil
}
