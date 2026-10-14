package tonwallet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type keySigner struct{ priv ed25519.PrivateKey }

func (k keySigner) SignWalletTransaction(data []byte) ([]byte, error) {
	return ed25519.Sign(k.priv, data), nil
}
func (k keySigner) GetWalletPublicKey() ([]byte, error) {
	return k.priv.Public().(ed25519.PublicKey), nil
}

func newSigner(t testing.TB) keySigner {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return keySigner{priv}
}

func TestAddressMatchesReferenceWallet(t *testing.T) {
	s := newSigner(t)
	for _, net := range []Network{Mainnet, Testnet} {
		w, err := New(s, net, "")
		require.NoError(t, err)

		ref, err := wallet.FromPrivateKeyWithOptions(s.priv, versionConfig(net))
		require.NoError(t, err)
		assert.Equal(t, ref.Address().StringRaw(), w.RawAddress(), "same key must give the same v5r1 account as a standard wallet app")

		friendly := w.Address()
		assert.Len(t, friendly, 48)
		if net == Mainnet {
			assert.True(t, strings.HasPrefix(friendly, "UQ"), friendly)
		} else {
			assert.True(t, strings.HasPrefix(friendly, "0Q"), friendly)
		}
		back, err := w.ParseRecipient(friendly)
		require.NoError(t, err)
		assert.Equal(t, w.RawAddress(), back.StringRaw())
	}
}

func TestSignerSignsMessageHash(t *testing.T) {
	s := newSigner(t)
	w, err := New(s, Testnet, "")
	require.NoError(t, err)
	c := cell.BeginCell().MustStoreUInt(42, 32).EndCell()
	sig, err := w.signer.SignWalletTransaction(c.Hash())
	require.NoError(t, err)
	assert.True(t, ed25519.Verify(w.pub, c.Hash(), sig))
}

func TestParseAndFormatTON(t *testing.T) {
	cases := map[string]string{
		"1":            "1000000000",
		"0.5":          "500000000",
		"0,25":         "250000000",
		".1":           "100000000",
		"12.000000001": "12000000001",
		"0.000000001":  "1",
	}
	for in, want := range cases {
		n, err := ParseTON(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, n.String(), in)
	}
	for _, bad := range []string{"", "0", "-1", "+1", "1.0000000001", "abc", "1.2.3", "0.000"} {
		_, err := ParseTON(bad)
		assert.ErrorIs(t, err, ErrBadAmount, bad)
	}
	assert.Equal(t, "1.5", FormatTON(big.NewInt(1_500_000_000)))
	assert.Equal(t, "0.000000001", FormatTON(big.NewInt(1)))
	assert.Equal(t, "12", FormatTON(big.NewInt(12_000_000_000)))
	assert.Equal(t, "-0.25", FormatTON(big.NewInt(-250_000_000)))
}

func TestTopUpLink(t *testing.T) {
	link := TopUpLink("UQabc", big.NewInt(500_000_000), "echofog top-up")
	assert.Equal(t, "ton://transfer/UQabc?amount=500000000&text=echofog+top-up", link)
	assert.Equal(t, "ton://transfer/UQabc", TopUpLink("UQabc", nil, ""))
}

func TestRecipientValidation(t *testing.T) {
	s := newSigner(t)
	main, _ := New(s, Mainnet, "")
	test, _ := New(s, Testnet, "")

	_, err := main.ParseRecipient("not-an-address")
	assert.ErrorIs(t, err, ErrBadAddress)
	_, err = main.ParseRecipient(test.Address()) // 0Q… is testnet-only
	assert.ErrorIs(t, err, ErrWrongNetwork)
	raw, err := main.ParseRecipient(main.RawAddress())
	require.NoError(t, err)
	assert.Equal(t, main.RawAddress(), raw.StringRaw())

	main.SetFrozen(true)
	_, err = main.Transfer(context.Background(), TransferRequest{To: main.Address(), Nano: big.NewInt(1)})
	assert.ErrorIs(t, err, ErrFrozen)
	main.SetFrozen(false)
	_, err = main.Transfer(context.Background(), TransferRequest{To: main.Address(), Nano: big.NewInt(0)})
	assert.ErrorIs(t, err, ErrBadAmount)
	_, err = main.Transfer(context.Background(), TransferRequest{To: main.Address(), Nano: big.NewInt(1), Comment: strings.Repeat("x", 121)})
	assert.Error(t, err)
}

// TestLiveTestnet talks to real TON testnet liteservers. Run with TON_LIVE=1.
func TestLiveTestnet(t *testing.T) {
	if os.Getenv("TON_LIVE") == "" {
		t.Skip("set TON_LIVE=1 to query TON testnet")
	}
	dir := t.TempDir()
	w, err := New(newSigner(t), Testnet, dir)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bal, deployed, err := w.Balance(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), bal.Int64(), "a fresh key has an empty account")
	assert.False(t, deployed)

	hist, err := w.History(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, hist)

	_, err = w.Transfer(ctx, TransferRequest{To: w.Address(), Nano: big.NewInt(100_000_000)})
	assert.ErrorIs(t, err, ErrInsufficientFunds, "nothing is broadcast without funds")

	_, err = os.Stat(filepath.Join(dir, "ton-testnet-global.config.json"))
	require.NoError(t, err, "liteserver config is cached for offline starts")

	// ton.org unreachable: the cached config still connects
	orig := configURLs[Testnet]
	configURLs[Testnet] = "http://127.0.0.1:1/blocked.json"
	defer func() { configURLs[Testnet] = orig }()
	w2, err := New(newSigner(t), Testnet, dir)
	require.NoError(t, err)
	_, _, err = w2.Balance(ctx)
	require.NoError(t, err)

	// a known busy testnet account has history we can parse
	t.Logf("fresh testnet wallet %s", w.Address())
}
