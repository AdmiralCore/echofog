package exit_gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xssnick/tonutils-go/address"
)

const testNanoPerMB = 4000 // about $0.01/GB at $2.5 per TON

func rawAddr(b byte) *address.Address {
	return address.MustParseRawAddr("0:" + hex.EncodeToString(bytes.Repeat([]byte{b}, 32)))
}

// fakeChain behaves like the vault contract: it holds deposits and pays claims the same way.
type fakeChain struct {
	mu       sync.Mutex
	vaults   map[string]*tonwallet.VaultInfo
	claimed  map[string]*big.Int // vault -> paid to the exit
	pubs     map[string]ed25519.PublicKey
	sent     [][]tonwallet.SignedVoucher
	down     bool
	sendFail error
}

func newFakeChain() *fakeChain {
	return &fakeChain{vaults: map[string]*tonwallet.VaultInfo{}, claimed: map[string]*big.Int{}, pubs: map[string]ed25519.PublicKey{}}
}

func (c *fakeChain) ReadVault(_ context.Context, v *address.Address) (tonwallet.VaultInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return tonwallet.VaultInfo{}, errors.New("liteservers unreachable")
	}
	if info := c.vaults[v.StringRaw()]; info != nil {
		cp := *info
		cp.Deposit = new(big.Int).Set(info.Deposit)
		return cp, nil
	}
	return tonwallet.VaultInfo{Deposit: new(big.Int)}, nil
}

func (c *fakeChain) VaultClaimed(_ context.Context, v, _ *address.Address) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return nil, errors.New("liteservers unreachable")
	}
	if n := c.claimed[v.StringRaw()]; n != nil {
		return new(big.Int).Set(n), nil
	}
	return new(big.Int), nil
}

func (c *fakeChain) SendClaims(_ context.Context, list []tonwallet.SignedVoucher) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendFail != nil {
		return nil, c.sendFail
	}
	c.sent = append(c.sent, list)
	for _, sv := range list {
		key := sv.Vault.StringRaw()
		info := c.vaults[key]
		if info == nil || !tonwallet.VerifyVoucher(c.pubs[key], sv.Voucher, sv.Signature) {
			continue // the contract would refuse it
		}
		already := c.claimed[key]
		if already == nil {
			already = new(big.Int)
		}
		delta := new(big.Int).Sub(sv.Cumulative, already)
		if delta.Sign() <= 0 {
			continue
		}
		if delta.Cmp(info.Deposit) > 0 {
			delta = new(big.Int).Set(info.Deposit) // partial payout, as the contract does
		}
		info.Deposit.Sub(info.Deposit, delta)
		c.claimed[key] = new(big.Int).Add(already, delta)
	}
	return []string{"hash"}, nil
}

// testUser is a client with its own vault.
type testUser struct {
	priv  ed25519.PrivateKey
	owner *address.Address
	vault *address.Address
}

func newTestUser(t *testing.T, seed byte, chain *fakeChain, deposit int64) testUser {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))
	owner := rawAddr(seed)
	vault, err := tonwallet.VaultAddress(owner, priv.Public().(ed25519.PublicKey))
	require.NoError(t, err)
	if chain != nil && deposit > 0 {
		chain.mu.Lock()
		chain.vaults[vault.StringRaw()] = &tonwallet.VaultInfo{Deployed: true, Deposit: big.NewInt(deposit)}
		chain.pubs[vault.StringRaw()] = priv.Public().(ed25519.PublicKey)
		chain.mu.Unlock()
	}
	return testUser{priv: priv, owner: owner, vault: vault}
}

func (u testUser) offer(t *testing.T, globalID int32, node *address.Address, cumulative int64) VoucherOffer {
	v := tonwallet.Voucher{GlobalID: globalID, Vault: u.vault, Node: node, Cumulative: big.NewInt(cumulative)}
	h, err := v.Hash()
	require.NoError(t, err)
	return VoucherOffer{
		Version:    voucherOfferVersion,
		Owner:      u.owner.StringRaw(),
		PubKey:     hex.EncodeToString(u.priv.Public().(ed25519.PublicKey)),
		Cumulative: big.NewInt(cumulative).String(),
		Sig:        hex.EncodeToString(ed25519.Sign(u.priv, h)),
	}
}

var exitPayout = rawAddr(0xEE)

func newTestBook(chain *fakeChain, path string) *VoucherBook {
	b := NewVoucherBook(path, tonwallet.MainnetGlobalID, exitPayout, chain)
	b.SetQuote(testNanoPerMB)
	return b
}

func TestVoucherCreditsWhatItNewlyPaysFor(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	u := newTestUser(t, 1, chain, 1_000_000_000)

	r := b.Accept(context.Background(), "tag-1", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 64*testNanoPerMB))
	require.True(t, r.OK, r.Error)
	assert.Equal(t, int64(64<<20), r.Credit, "64 MB paid for")
	assert.Equal(t, int64(64<<20+VoucherOverdraftBytes), b.Available("tag-1"), "plus the overdraft of a vault that has paid")

	// the same voucher again (a lost reply, say) credits nothing more; an older one neither
	r = b.Accept(context.Background(), "tag-1", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 64*testNanoPerMB))
	require.True(t, r.OK)
	assert.Equal(t, int64(64<<20), r.Credit)
	r = b.Accept(context.Background(), "tag-1", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 10*testNanoPerMB))
	require.True(t, r.OK)
	assert.Equal(t, int64(64<<20), r.Credit)

	// traffic uses it up, the next voucher adds only its difference
	assert.Equal(t, int64(60<<20), b.Spend("tag-1", 60<<20))
	r = b.Accept(context.Background(), "tag-1", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 128*testNanoPerMB))
	require.True(t, r.OK)
	assert.Equal(t, int64(68<<20), r.Credit)
	assert.Equal(t, big.NewInt(128*testNanoPerMB).String(), r.Cumulative)

	// what is spent cannot exceed what is there, overdraft included
	assert.Equal(t, int64(68<<20+VoucherOverdraftBytes), b.Spend("tag-1", 200<<20))
	assert.Zero(t, b.Available("tag-1"))
	assert.Zero(t, b.Spend("tag-1", 1))
	assert.Equal(t, int64(-VoucherOverdraftBytes), b.Terms("tag-1").Credit)
	assert.Zero(t, b.Terms("tag-1").OverdraftLeft)

	// the next voucher pays the overdraft back first
	r = b.Accept(context.Background(), "tag-1", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 192*testNanoPerMB))
	require.True(t, r.OK)
	assert.Equal(t, int64(64<<20-VoucherOverdraftBytes), r.Credit)
}

// A vault that has never paid gets no overdraft, nor one that is being withdrawn.
func TestVoucherOverdraftOnlyForAPayingVault(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	assert.Zero(t, b.Available("nobody"))
	assert.Zero(t, b.Spend("nobody", 1))

	u := newTestUser(t, 1, chain, 1_000_000_000)
	require.True(t, b.Accept(context.Background(), "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, testNanoPerMB)).OK)
	assert.Equal(t, int64(VoucherOverdraftBytes), b.Terms("t").OverdraftLeft)

	b.mu.Lock()
	b.data.Accounts[u.vault.StringRaw()].WithdrawAt = 1
	b.mu.Unlock()
	assert.Equal(t, int64(bytesPerMB), b.Available("t"), "no overdraft while the deposit is being withdrawn")
	assert.Zero(t, b.Terms("t").OverdraftLeft)
}

func TestVoucherRejections(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	u := newTestUser(t, 1, chain, 1_000_000_000)
	ctx := context.Background()

	forged := u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 1000)
	forged.Cumulative = "2000"
	assert.Contains(t, b.Accept(ctx, "t", forged).Error, "signature")

	assert.Contains(t, b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, rawAddr(0xAB), 1000)).Error, "signature", "a voucher for another exit")
	assert.Contains(t, b.Accept(ctx, "t", u.offer(t, tonwallet.TestnetGlobalID, exitPayout, 1000)).Error, "signature", "a testnet voucher on mainnet")

	other := newTestUser(t, 2, nil, 0)
	stolen := u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 1000)
	stolen.PubKey = hex.EncodeToString(other.priv.Public().(ed25519.PublicKey))
	assert.False(t, b.Accept(ctx, "t", stolen).OK, "someone else's key for this vault")

	for _, bad := range []VoucherOffer{
		{Version: 9},
		{Version: 1, Owner: "nope"},
		{Version: 1, Owner: u.owner.StringRaw(), PubKey: "00"},
		{Version: 1, Owner: u.owner.StringRaw(), PubKey: hex.EncodeToString(u.priv.Public().(ed25519.PublicKey)), Sig: "00"},
	} {
		assert.False(t, b.Accept(ctx, "t", bad).OK)
	}
	huge := u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 1)
	huge.Cumulative = new(big.Int).Lsh(big.NewInt(1), 125).String()
	assert.False(t, b.Accept(ctx, "t", huge).OK)

	// no vault, or an empty one
	ghost := newTestUser(t, 3, chain, 0)
	assert.Contains(t, b.Accept(ctx, "g", ghost.offer(t, tonwallet.MainnetGlobalID, exitPayout, 1000)).Error, "empty")

	// a voucher bigger than the deposit
	assert.Contains(t, b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 2_000_000_000)).Error, "too small")

	// no quote (the TON rate is unknown): no vouchers
	b.SetQuote(0)
	assert.Contains(t, b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 1000)).Error, "does not take")
}

func TestVoucherRefusedOnceTheVaultIsBeingWithdrawn(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	now := time.Unix(1_800_000_000, 0)
	b.now = func() time.Time { return now }
	u := newTestUser(t, 1, chain, 1_000_000_000)
	ctx := context.Background()

	require.True(t, b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 1000)).OK)

	chain.mu.Lock()
	chain.vaults[u.vault.StringRaw()].WithdrawAt = now.Add(tonwallet.VaultCooldown)
	chain.mu.Unlock()

	// a recent view answers at once (a withdrawal takes a day, the exit can still claim) and is
	// re-read in the background; after that the withdrawal is seen
	now = now.Add(2 * vaultCheckEvery)
	r := b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 2000))
	assert.True(t, r.OK, "no wait on the chain for a recently checked vault")
	require.Eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.data.Accounts[u.vault.StringRaw()].WithdrawAt != 0
	}, 2*time.Second, 10*time.Millisecond)
	r = b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 3000))
	assert.False(t, r.OK)
	assert.Contains(t, r.Error, "withdrawn")
	assert.Positive(t, r.Credit, "what was already bought stays usable")
}

func TestVoucherAcceptWhileTheChainIsDown(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	now := time.Unix(1_800_000_000, 0)
	b.now = func() time.Time { return now }
	u := newTestUser(t, 1, chain, 1_000_000_000)
	ctx := context.Background()

	chain.down = true
	assert.False(t, b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 1000)).OK, "never seen and cannot check")

	chain.down = false
	require.True(t, b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 1000)).OK)

	chain.down = true
	now = now.Add(5 * time.Minute)
	assert.True(t, b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 2000)).OK, "a recent view is good enough")
	now = now.Add(vaultStaleOK)
	assert.False(t, b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 3000)).OK, "too old a view")
}

func TestVoucherBookSurvivesARestart(t *testing.T) {
	chain := newFakeChain()
	path := filepath.Join(t.TempDir(), "vouchers.json")
	b := newTestBook(chain, path)
	u := newTestUser(t, 1, chain, 1_000_000_000)
	require.True(t, b.Accept(context.Background(), "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 64*testNanoPerMB)).OK)
	b.Spend("t", 4<<20)
	require.NoError(t, b.Flush())

	again := newTestBook(chain, path)
	assert.Equal(t, int64(60<<20+VoucherOverdraftBytes), again.Available("t"))
	terms := again.Terms("t")
	require.NotNil(t, terms)
	assert.Equal(t, big.NewInt(64*testNanoPerMB).String(), terms.Cumulative)
	sig, err := hex.DecodeString(terms.Sig)
	require.NoError(t, err)
	v := tonwallet.Voucher{GlobalID: tonwallet.MainnetGlobalID, Vault: u.vault, Node: exitPayout, Cumulative: big.NewInt(64 * testNanoPerMB)}
	assert.True(t, tonwallet.VerifyVoucher(u.priv.Public().(ed25519.PublicKey), v, sig), "the stored voucher can still be cashed")
	assert.Equal(t, big.NewInt(64*testNanoPerMB), again.Owed())
}

func testPolicy() ClaimPolicy {
	p := DefaultClaimPolicy()
	p.Tick = time.Minute
	return p
}

func TestClaimPolicy(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	now := time.Unix(1_800_000_000, 0)
	b.now = func() time.Time { return now }
	ctx := context.Background()
	p := testPolicy()
	b.data.LastClaim = now.Unix()

	small := newTestUser(t, 1, chain, 1_000_000_000)
	mid := newTestUser(t, 2, chain, 1_000_000_000)
	require.True(t, b.Accept(ctx, "s", small.offer(t, tonwallet.MainnetGlobalID, exitPayout, 5_000_000)).OK) // 0.005 TON
	require.True(t, b.Accept(ctx, "m", mid.offer(t, tonwallet.MainnetGlobalID, exitPayout, 30_000_000)).OK)  // 0.03 TON

	// nothing urgent, under the batch total, not yet time: wait
	n, err := b.ClaimOnce(ctx, p)
	require.NoError(t, err)
	assert.Zero(t, n)

	// one client owes past the urgent limit: cash now, but only what is worth the gas
	require.True(t, b.Accept(ctx, "m", mid.offer(t, tonwallet.MainnetGlobalID, exitPayout, 60_000_000)).OK)
	n, err = b.ClaimOnce(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the 0.005 TON voucher waits")
	assert.Equal(t, mid.vault.StringRaw(), chain.sent[0][0].Vault.StringRaw())
	assert.Equal(t, big.NewInt(60_000_000), chain.claimed[mid.vault.StringRaw()])

	// not sent twice while the claim settles; afterwards the chain's total is read back
	n, _ = b.ClaimOnce(ctx, p)
	assert.Zero(t, n)
	now = now.Add(claimSettle + p.Tick)
	n, _ = b.ClaimOnce(ctx, p)
	assert.Zero(t, n)
	assert.Equal(t, big.NewInt(5_000_000), b.Owed(), "only the small one is still owed")
	assert.Equal(t, big.NewInt(60_000_000), b.Cashed())

	// a vault being withdrawn is cashed even for a small amount, before it is emptied
	chain.mu.Lock()
	chain.vaults[small.vault.StringRaw()].WithdrawAt = now.Add(tonwallet.VaultCooldown)
	chain.mu.Unlock()
	now = now.Add(p.RefreshEvery)
	n, err = b.ClaimOnce(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, big.NewInt(5_000_000), chain.claimed[small.vault.StringRaw()])
}

func TestClaimsWhenTheBatchAddsUpOrTheTimeComes(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	now := time.Unix(1_800_000_000, 0)
	b.now = func() time.Time { return now }
	b.data.LastClaim = now.Unix()
	ctx := context.Background()
	p := testPolicy()

	var users []testUser
	for i := 0; i < 5; i++ {
		u := newTestUser(t, byte(10+i), chain, 1_000_000_000)
		users = append(users, u)
		require.True(t, b.Accept(ctx, "u"+string(rune('a'+i)), u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 30_000_000)).OK)
	}
	// 5 x 0.03 = 0.15 TON: under the batch total, nobody urgent
	n, _ := b.ClaimOnce(ctx, p)
	assert.Zero(t, n)

	// two more make it 0.21 TON: the whole batch goes
	for i := 5; i < 7; i++ {
		u := newTestUser(t, byte(10+i), chain, 1_000_000_000)
		require.True(t, b.Accept(ctx, "u"+string(rune('a'+i)), u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 30_000_000)).OK)
	}
	n, _ = b.ClaimOnce(ctx, p)
	assert.Equal(t, 7, n)

	// later, a little more from each: under the total, but the period is over
	now = now.Add(claimSettle + p.Tick)
	_, _ = b.ClaimOnce(ctx, p)
	for i, u := range users {
		require.True(t, b.Accept(ctx, "u"+string(rune('a'+i)), u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 55_000_000)).OK)
	}
	n, _ = b.ClaimOnce(ctx, p)
	assert.Zero(t, n)
	now = now.Add(p.Every)
	n, _ = b.ClaimOnce(ctx, p)
	assert.Equal(t, 5, n)
}

func TestClaimFailureIsRetried(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	ctx := context.Background()
	u := newTestUser(t, 1, chain, 1_000_000_000)
	require.True(t, b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 80_000_000)).OK)

	chain.sendFail = tonwallet.ErrInsufficientFunds
	_, err := b.ClaimOnce(ctx, testPolicy())
	assert.ErrorIs(t, err, tonwallet.ErrInsufficientFunds)
	chain.sendFail = nil
	n, err := b.ClaimOnce(ctx, testPolicy())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestPartialPayoutIsFinishedAfterATopUp(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	now := time.Unix(1_800_000_000, 0)
	b.now = func() time.Time { return now }
	ctx := context.Background()
	p := testPolicy()
	u := newTestUser(t, 1, chain, 100_000_000)
	require.True(t, b.Accept(ctx, "t", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 90_000_000)).OK)

	// the user also spent the deposit elsewhere: only 0.04 TON is left when we claim
	chain.mu.Lock()
	chain.vaults[u.vault.StringRaw()].Deposit = big.NewInt(40_000_000)
	chain.mu.Unlock()
	n, _ := b.ClaimOnce(ctx, p)
	require.Equal(t, 1, n)
	assert.Equal(t, big.NewInt(40_000_000), chain.claimed[u.vault.StringRaw()])

	now = now.Add(claimSettle + p.Tick)
	_, _ = b.ClaimOnce(ctx, p)
	assert.Equal(t, big.NewInt(50_000_000), b.Owed(), "the rest is still owed")

	chain.mu.Lock()
	chain.vaults[u.vault.StringRaw()].Deposit = big.NewInt(500_000_000)
	chain.mu.Unlock()
	now = now.Add(p.RefreshEvery)
	n, _ = b.ClaimOnce(ctx, p)
	require.Equal(t, 1, n)
	assert.Equal(t, big.NewInt(90_000_000), chain.claimed[u.vault.StringRaw()], "the same voucher pays the rest")
}

func TestVoucherPaidTunnel(t *testing.T) {
	target := echoServer(t)
	svc, _ := paidExit(t, 0)
	chain := newFakeChain()
	book := newTestBook(chain, "")
	svc.SetVouchers(book)
	u := newTestUser(t, 1, chain, 1_000_000_000)

	open := func(context.Context) (FramedStream, error) {
		client, server := newPipePair()
		go func() {
			typ, payload, err := server.ReadFrame()
			if err != nil {
				return
			}
			if typ == net_types.TypeExitVoucher {
				svc.HandleVoucherFrom(context.Background(), server, payload, testClient)
				return
			}
			svc.HandleTunnelFrom(context.Background(), server, payload, testClient)
		}()
		return client, nil
	}
	d := NewP2PDialer(open)

	// nothing paid: refused
	_, err := d.DialExitStreamConfirmed(context.Background(), target)
	assert.ErrorIs(t, err, ErrPaymentRequired)

	info := svc.AccountOf(testClient)
	require.True(t, info.TakesVouchers())
	assert.Equal(t, int64(testNanoPerMB), info.Vault.NanoPerMB)
	assert.Equal(t, exitPayout.StringRaw(), info.Vault.Node)

	// one voucher for 1 MB, then traffic
	r, err := d.SendVoucher(context.Background(), u.offer(t, tonwallet.MainnetGlobalID, exitPayout, testNanoPerMB))
	require.NoError(t, err)
	require.True(t, r.OK, r.Error)
	assert.Equal(t, int64(bytesPerMB), r.Credit)
	assert.Equal(t, int64(bytesPerMB), svc.AccountOf(testClient).Remaining())

	c, err := d.DialExitStream(context.Background(), target)
	require.NoError(t, err)
	_, err = c.Write(make([]byte, 50_000))
	require.NoError(t, err)
	_, err = io.ReadFull(c, make([]byte, 50_000))
	require.NoError(t, err)
	c.Close()
	require.Eventually(t, func() bool {
		return book.Available(PayTag(testClient, testExit)) < VoucherOverdraftBytes+bytesPerMB-90_000
	}, 2*time.Second, 10*time.Millisecond)

	// a free-listed peer is told so instead of being charged
	svc.SetFreePeers([]types.PeerID{testClient})
	r, err = d.SendVoucher(context.Background(), u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 2*testNanoPerMB))
	require.NoError(t, err)
	assert.False(t, r.OK)
	assert.Contains(t, r.Error, "free")
	assert.Nil(t, svc.AccountOf(testClient).Vault)
}

// A voucher is public once it has been claimed on-chain. Replaying it from another peer must not
// tie that peer to the victim's vault: it would spend the victim's credit and read its terms.
func TestVoucherReplayFromAnotherPeerDoesNotBind(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	u := newTestUser(t, 1, chain, 1_000_000_000)
	ctx := context.Background()

	old := u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 64*testNanoPerMB)
	require.True(t, b.Accept(ctx, "victim", old).OK)
	require.True(t, b.Accept(ctx, "victim", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 128*testNanoPerMB)).OK)

	for _, replay := range []VoucherOffer{old, u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 128*testNanoPerMB)} {
		r := b.Accept(ctx, "attacker", replay)
		assert.False(t, r.OK, "an old or repeated voucher from someone else is refused")
		assert.Contains(t, r.Error, "already used")
	}
	assert.Zero(t, b.Available("attacker"))
	assert.Zero(t, b.Spend("attacker", 1<<20))
	if tm := b.Terms("attacker"); tm != nil {
		assert.Empty(t, tm.Cumulative+tm.Sig, "the victim's voucher is not shown to the attacker")
		assert.Zero(t, tm.Credit)
	}
	assert.Equal(t, int64(128<<20+VoucherOverdraftBytes), b.Available("victim"), "the victim keeps the credit (and its overdraft)")

	// the owner reconnecting under a new peer ID binds again with its next voucher
	require.True(t, b.Accept(ctx, "victim-2", u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 129*testNanoPerMB)).OK)
	assert.Positive(t, b.Available("victim-2"))
}

// An empty node wallet cannot pay the fee for cashing vouchers: the book says so for the stats
// and the app, warns once an hour rather than every round, and clears it once cashing works.
func TestClaimProblemIsReportedAndCleared(t *testing.T) {
	chain := newFakeChain()
	b := newTestBook(chain, "")
	clock := time.Unix(1_800_000_000, 0)
	b.now = func() time.Time { return clock }

	b.reportClaim(fmt.Errorf("%w: collecting vouchers needs gas", tonwallet.ErrInsufficientFunds))
	assert.Contains(t, b.ClaimProblem(), "no TON")
	assert.False(t, b.noteClaimResult(b.ClaimProblem()), "a second failure within the hour is not logged again")

	clock = clock.Add(claimWarnEvery + time.Second)
	assert.True(t, b.noteClaimResult(b.ClaimProblem()), "but it is after an hour")

	b.reportClaim(nil)
	assert.Empty(t, b.ClaimProblem(), "cashing worked: nothing to report")
}
