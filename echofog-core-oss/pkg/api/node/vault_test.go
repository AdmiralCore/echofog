package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
	"github.com/echofog/echofog-core/pkg/api/types"
	pnode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xssnick/tonutils-go/address"
)

// --- the pure parts ---

func vaultAccount(remainingMB int64, nanoPerMB int64) exit_gateway.AccountInfo {
	info := accountWith(0)
	info.Credit = 0
	info.Vault = &exit_gateway.VaultTerms{NanoPerMB: nanoPerMB, Node: "0:" + hex.EncodeToString(bytes.Repeat([]byte{0xEE}, 32)), GlobalID: tonwallet.MainnetGlobalID, Credit: remainingMB << 20}
	return info
}

func TestVoucherChunksAreSmallAndBoughtAhead(t *testing.T) {
	d := decideVoucher(vaultAccount(40, 20_000), settings(true), 0, t0, time.Time{})
	assert.Equal(t, payNothing, d.Action, "40 MB left is enough")

	d = decideVoucher(vaultAccount(10, 20_000), settings(true), 0, t0, time.Time{})
	require.Equal(t, payNow, d.Action)
	assert.Equal(t, int64(price*voucherChunkMB), d.Micro, "one 64 MB chunk, about a third of a cent")

	d = decideVoucher(vaultAccount(10, 20_000), settings(false), 0, t0, time.Time{})
	assert.Equal(t, payAsk, d.Action, "no automatic payments without consent, vouchers included")

	d = decideVoucher(vaultAccount(10, 20_000), settings(true), 5*microPerUSDT, t0, time.Time{})
	assert.Equal(t, payNothing, d.Action)
	assert.Contains(t, d.Reason, "daily limit")

	noVault := accountWith(0)
	assert.Equal(t, payNothing, decideVoucher(noVault, settings(true), 0, t0, time.Time{}).Action)
}

func TestExitQuoteMustMatchOurRate(t *testing.T) {
	// 49 micro-USD per MB at $2.50 per TON is 19,600 nanoton per MB
	assert.True(t, fairQuote(vaultAccount(0, 19_600), 2.5))
	assert.True(t, fairQuote(vaultAccount(0, 20_500), 2.5), "within 5%")
	assert.False(t, fairQuote(vaultAccount(0, 21_000), 2.5), "more than 5% above our rate")
	assert.False(t, fairQuote(vaultAccount(0, 19_600), 0), "no rate, no vouchers")
}

func TestVoucherAmountBuysAtLeastTheChunk(t *testing.T) {
	info := vaultAccount(0, 19_601)
	micro := int64(price * voucherChunkMB)
	nano := voucherNano(info, micro)
	credited := new(big.Int).Mul(nano, big.NewInt(1<<20))
	credited.Quo(credited, big.NewInt(info.Vault.NanoPerMB))
	assert.GreaterOrEqual(t, credited.Int64(), info.BytesFor(micro))
}

func TestGuardPaysByVoucherAndDoesNotWait(t *testing.T) {
	now := t0
	ex := &fakeExit{info: vaultAccount(1, 20_000)}
	p := newGuard(true, &now)
	ctx := context.Background()
	var vouchers []int64
	env := ex.env()
	env.vault = func(context.Context, *exit_gateway.AccountInfo) bool { return true }
	env.payVoucher = func(_ context.Context, micro int64, _ *exit_gateway.AccountInfo) error {
		vouchers = append(vouchers, micro)
		return nil
	}

	wait := p.step(ctx, env)
	assert.Equal(t, []int64{price * voucherChunkMB}, vouchers)
	assert.Empty(t, ex.paid, "no on-chain transfer")
	assert.Equal(t, voucherGuardEvery, wait)
	assert.False(t, p.status.Pending)
	assert.True(t, p.status.PaysByVault)

	// refused (say the vault is being withdrawn): back off, then try again
	env.payVoucher = func(context.Context, int64, *exit_gateway.AccountInfo) error { return errors.New("the vault cannot pay") }
	p.step(ctx, env)
	assert.Contains(t, p.status.LastError, "vault cannot pay")
	n := len(vouchers)
	now = now.Add(time.Second)
	p.step(ctx, env)
	assert.Len(t, vouchers, n, "no retry within the back-off")
}

// --- paying a real exit service through the voucher stream ---

type chanFrame struct {
	t net_types.MessageType
	b []byte
}

type chanStream struct {
	in, out chan chanFrame
}

func chanPair() (*chanStream, *chanStream) {
	a, b := make(chan chanFrame, 4), make(chan chanFrame, 4)
	return &chanStream{in: b, out: a}, &chanStream{in: a, out: b}
}

func (c *chanStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *chanStream) Write(p []byte) (int, error) { return len(p), nil }
func (c *chanStream) Close() error                { return nil }
func (c *chanStream) WriteFrame(t net_types.MessageType, d []byte) error {
	c.out <- chanFrame{t, append([]byte(nil), d...)}
	return nil
}
func (c *chanStream) ReadFrame() (net_types.MessageType, []byte, error) {
	f, ok := <-c.in
	if !ok {
		return 0, nil, io.EOF
	}
	return f.t, f.b, nil
}

// depositChain says every vault holds 1 TON.
type depositChain struct{}

func (depositChain) ReadVault(context.Context, *address.Address) (tonwallet.VaultInfo, error) {
	return tonwallet.VaultInfo{Deployed: true, Deposit: big.NewInt(1_000_000_000)}, nil
}
func (depositChain) VaultClaimed(context.Context, *address.Address, *address.Address) (*big.Int, error) {
	return new(big.Int), nil
}
func (depositChain) SendClaims(context.Context, []tonwallet.SignedVoucher) ([]string, error) {
	return nil, nil
}

type testKey struct{ priv ed25519.PrivateKey }

func (k testKey) SignWalletTransaction(d []byte) ([]byte, error) { return ed25519.Sign(k.priv, d), nil }
func (k testKey) GetWalletPublicKey() ([]byte, error)            { return k.priv.Public().(ed25519.PublicKey), nil }

func TestPayByVoucherKeepsOneRunningTotal(t *testing.T) {
	dir := t.TempDir()
	w, err := tonwallet.New(testKey{ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))}, tonwallet.Mainnet, "")
	require.NoError(t, err)
	cfg := &config.AppConfig{}
	cfg.Storage.DatabasePath = filepath.Join(dir, "db")
	n := &NodeApi{node: &pnode.Node{TonWallet: w, Cfg: cfg}}

	payout := address.MustParseRawAddr("0:" + hex.EncodeToString(bytes.Repeat([]byte{0xEE}, 32)))
	exitSvc := exit_gateway.NewExitService()
	exitSvc.SetEnabled(true)
	exitSvc.SetPolicy(exit_gateway.Policy{PricePerMB: price})
	exitSvc.SetPayments(exit_gateway.NewLedger(""), types.PeerID{10}, "UQ-exit")
	book := exit_gateway.NewVoucherBook(filepath.Join(dir, "exit.json"), tonwallet.MainnetGlobalID, payout, depositChain{})
	book.SetQuote(19_600)
	exitSvc.SetVouchers(book)
	client := types.PeerID{1}

	dialer := exit_gateway.NewP2PDialer(func(context.Context) (exit_gateway.FramedStream, error) {
		c, s := chanPair()
		go func() {
			if typ, payload, err := s.ReadFrame(); err == nil && typ == net_types.TypeExitVoucher {
				exitSvc.HandleVoucherFrom(context.Background(), s, payload, client)
			}
		}()
		return c, nil
	})
	ctx := context.Background()
	chunk := int64(price * voucherChunkMB)

	info := exitSvc.AccountOf(client)
	require.True(t, info.TakesVouchers())
	require.NoError(t, n.payByVoucher(ctx, dialer, &info, chunk))
	info = exitSvc.AccountOf(client)
	require.NoError(t, n.payByVoucher(ctx, dialer, &info, chunk))
	info = exitSvc.AccountOf(client)
	assert.GreaterOrEqual(t, info.Vault.Credit, int64(2*voucherChunkMB<<20), "two chunks credited")
	held, _ := new(big.Int).SetString(info.Vault.Cumulative, 10)

	// the client loses its record: it continues from the exit's copy, which carries its signature
	n.vault = vaultClient{}
	_ = os.Remove(filepath.Join(dir, "ton", "vouchers-signed.json"))
	require.NoError(t, n.payByVoucher(ctx, dialer, &info, chunk))
	after := exitSvc.AccountOf(client)
	assert.GreaterOrEqual(t, after.Vault.Credit, int64(3*voucherChunkMB<<20), "the third chunk is paid, not swallowed")
	now, _ := new(big.Int).SetString(after.Vault.Cumulative, 10)
	assert.Equal(t, voucherNano(info, chunk), new(big.Int).Sub(now, held))

	// an exit claiming a bigger total without our signature is ignored
	lying := after
	lying.Vault = &exit_gateway.VaultTerms{}
	*lying.Vault = *after.Vault
	lying.Vault.Cumulative = new(big.Int).Mul(now, big.NewInt(100)).String()
	n.vault = vaultClient{}
	_ = os.Remove(filepath.Join(dir, "ton", "vouchers-signed.json"))
	err = n.payByVoucher(ctx, dialer, &lying, chunk)
	require.NoError(t, err, "the exit still holds our real voucher, so a fresh small one is simply older")
	final := exitSvc.AccountOf(client)
	assert.Equal(t, after.Vault.Cumulative, final.Vault.Cumulative, "nothing signed on top of the invented amount")
}
