package exit_gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const accountInfoVersion = 1

// AccountInfo is what an exit tells one client about its payment account. It is the client's only
// source for where and how to pay, and for what is left of what it paid.
type AccountInfo struct {
	Version     int    `json:"v"`
	Enabled     bool   `json:"enabled"`
	PricePerMB  int64  `json:"price_per_mb"`            // micro-USDT per MB; 0 = free exit
	PayAddress  string `json:"pay_address,omitempty"`   // TON address to send USDT to
	Tag         string `json:"tag,omitempty"`           // goes into the transfer comment
	FreeLeft    int64  `json:"free_left"`               // free bytes left today
	Credit      int64  `json:"credit"`                  // paid bytes left; negative = overdraft owed
	MinPayMicro int64  `json:"min_pay_micro,omitempty"` // smaller transfers are not credited
	// Assets the exit takes: "USDT", and "TON" credited at the TON/USD rate when it arrives. An
	// exit that does not say takes USDT only.
	Assets []string `json:"assets,omitempty"`
	// Vault: the exit also takes vouchers from the client's TON vault (nil: it does not).
	Vault *VaultTerms `json:"vault,omitempty"`
}

// PayAssets are the assets an exit with payments takes.
var PayAssets = []string{"USDT", "TON"}

// Accepts tells whether the exit takes payments in asset.
func (a AccountInfo) Accepts(asset string) bool {
	if len(a.Assets) == 0 {
		return asset == "USDT"
	}
	for _, x := range a.Assets {
		if x == asset {
			return true
		}
	}
	return false
}

// Remaining is everything the client can still use, free and paid. Negative means it is in the
// overdraft; the exit cuts it at -OverdraftBytes.
func (a AccountInfo) Remaining() int64 {
	r := a.FreeLeft + a.Credit
	if a.Vault != nil {
		r += a.Vault.Credit
	}
	return r
}

// Exhausted reports that the exit has nothing left to serve this client with: no free traffic,
// no paid traffic and no overdraft to bridge the time until the next payment arrives.
func (a AccountInfo) Exhausted() bool {
	if !a.Enabled || a.PricePerMB <= 0 || a.Remaining() > 0 {
		return false
	}
	if a.Credit < 0 && a.Credit > -OverdraftBytes {
		return false // in the ledger overdraft: still served while a transfer confirms
	}
	return a.Vault == nil || a.Vault.OverdraftLeft <= 0
}

// TakesVouchers reports whether the exit can be paid from the client's vault.
func (a AccountInfo) TakesVouchers() bool {
	return a.Enabled && a.PricePerMB > 0 && a.Vault != nil && a.Vault.NanoPerMB > 0 && a.Vault.Node != ""
}

// CanPay reports whether this exit accepts payments at all.
func (a AccountInfo) CanPay() bool {
	return a.Enabled && a.PricePerMB > 0 && a.PayAddress != "" && a.Tag != ""
}

// BytesFor is how much traffic a payment of micro buys at this exit's price.
func (a AccountInfo) BytesFor(micro int64) int64 { return mulDiv(micro, bytesPerMB, a.PricePerMB) }

// AccountOf builds the account the exit would report for peer.
func (e *ExitService) AccountOf(peer types.PeerID) AccountInfo {
	e.policyMu.Lock()
	pol, l, v, self, addr := e.policy, e.ledger, e.vouchers, e.self, e.payAddress
	if e.freePeers[peer] {
		pol.PricePerMB = 0 // on the free list: this peer sees a free exit and never pays
	}
	var freeLeft int64
	if pol.PricePerMB > 0 {
		freeLeft = pol.FreeBytes - e.quotaLocked(peer).used
		if freeLeft < 0 {
			freeLeft = 0
		}
	}
	e.policyMu.Unlock()

	info := AccountInfo{Version: accountInfoVersion, Enabled: e.enabled.Load(), PricePerMB: pol.PricePerMB, FreeLeft: freeLeft}
	if l != nil && addr != "" && pol.PricePerMB > 0 {
		info.PayAddress = addr
		info.Tag = PayTag(peer, self)
		info.Credit, _ = l.Balance(info.Tag)
		info.MinPayMicro = MinPayMicro
		info.Assets = PayAssets
	}
	if v != nil && pol.PricePerMB > 0 {
		info.Vault = v.Terms(PayTag(peer, self))
	}
	return info
}

// HandleAccountFrom answers a TypeExitAccount request with the caller's account.
func (e *ExitService) HandleAccountFrom(_ context.Context, st FramedStream, peer types.PeerID) {
	defer st.Close()
	raw, err := json.Marshal(e.AccountOf(peer))
	if err != nil {
		return
	}
	_ = st.WriteFrame(net_types.TypeExitAccount, raw)
}

// QueryAccount asks the exit for this client's account.
func (d *P2PDialer) QueryAccount(ctx context.Context) (*AccountInfo, error) {
	if d == nil || d.open == nil {
		return nil, errNoStreamOpener
	}
	st, err := d.open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open stream to exit peer: %w", err)
	}
	defer st.Close()
	if err := st.WriteFrame(net_types.TypeExitAccount, nil); err != nil {
		return nil, err
	}
	type reply struct {
		t   net_types.MessageType
		b   []byte
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		t, b, err := st.ReadFrame()
		ch <- reply{t, b, err}
	}()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		if r.t != net_types.TypeExitAccount {
			return nil, ErrExitProtocol
		}
		var info AccountInfo
		if err := json.Unmarshal(r.b, &info); err != nil || info.Version != accountInfoVersion {
			return nil, ErrExitProtocol
		}
		return &info, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout.C:
		return nil, context.DeadlineExceeded
	}
}
