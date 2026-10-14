package tonwallet

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xssnick/tonutils-go/ton/wallet"
)

// liveSigner loads (or creates) the testnet-only phrase in TON_TEST_SEED_FILE, so live tests
// reuse one funded testnet wallet across runs.
func liveSigner(t *testing.T) keySigner {
	t.Helper()
	path := os.Getenv("TON_TEST_SEED_FILE")
	if path == "" {
		t.Skip("set TON_TEST_SEED_FILE to a file holding a testnet-only TON phrase")
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		raw = []byte(strings.Join(wallet.NewSeed(), " "))
		require.NoError(t, os.WriteFile(path, raw, 0o600))
	} else {
		require.NoError(t, err)
	}
	key, err := wallet.SeedToPrivateKeyWithOptions(strings.Fields(string(raw)), wallet.WithPassword(""), wallet.WithBIP39(false))
	require.NoError(t, err)
	return keySigner{ed25519.PrivateKey(key)}
}

func TestLiveTestnetAddress(t *testing.T) {
	w, err := New(liveSigner(t), Testnet, "")
	require.NoError(t, err)
	t.Logf("TESTNET ADDRESS: %s", w.Address())
}

// TestLiveTestnetSend makes a real testnet transfer from the funded TON_TEST_SEED_FILE wallet
// (first send also deploys the v5r1 contract). Run with TON_LIVE_SEND=1.
func TestLiveTestnetSend(t *testing.T) {
	if os.Getenv("TON_LIVE_SEND") == "" {
		t.Skip("set TON_LIVE_SEND=1 and fund the TON_TEST_SEED_FILE wallet with testnet TON")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	w, err := New(liveSigner(t), Testnet, t.TempDir())
	require.NoError(t, err)
	before, _, err := w.Balance(ctx)
	require.NoError(t, err)
	t.Logf("balance before: %s TON", FormatTON(before))
	require.True(t, before.Cmp(big.NewInt(200_000_000)) > 0, "fund %s with testnet TON first", w.Address())

	dest, err := New(newSigner(t), Testnet, "")
	require.NoError(t, err)
	amount := big.NewInt(50_000_000)
	comment := fmt.Sprintf("echofog live test %d", time.Now().Unix())
	hash, err := w.Transfer(ctx, TransferRequest{To: dest.Address(), Nano: amount, Comment: comment})
	require.NoError(t, err)
	t.Logf("broadcast external message %s", hash)

	var sent *Tx
	require.Eventually(t, func() bool {
		hist, err := w.History(ctx, 10)
		if err != nil {
			return false
		}
		for i := range hist {
			if !hist[i].Incoming && hist[i].Comment == comment {
				sent = &hist[i]
				return true
			}
		}
		return false
	}, 2*time.Minute, 3*time.Second, "transfer never appeared on chain")
	assert.Equal(t, amount.String(), sent.AmountNano)
	assert.False(t, sent.Failed)

	after, deployed, err := w.Balance(ctx)
	require.NoError(t, err)
	assert.True(t, deployed, "first transfer deploys the wallet contract")
	spent := new(big.Int).Sub(before, after)
	assert.True(t, spent.Cmp(amount) >= 0 && spent.Cmp(new(big.Int).Add(amount, FeeReserve)) <= 0, "spent %s", FormatTON(spent))
	t.Logf("tx %s, spent %s TON (fee %s)", sent.Hash, FormatTON(spent), FormatTON(new(big.Int).Sub(spent, amount)))

	got, _, err := dest.Balance(ctx)
	require.NoError(t, err)
	assert.Equal(t, amount.String(), got.String(), "recipient received the exact amount")
}

// TestLiveTestnetReadOnly reaches the testnet lite servers and reads a wallet's state: balance,
// history and the USDT balance (USDT is not on testnet, which the wallet must say plainly). It
// needs no funds. Run with TON_LIVE=1.
func TestLiveTestnetReadOnly(t *testing.T) {
	if os.Getenv("TON_LIVE") == "" {
		t.Skip("set TON_LIVE=1 to query TON testnet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	w, err := New(newSigner(t), Testnet, t.TempDir())
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(w.Address(), "0Q"), "testnet addresses are non-bounceable 0Q…: %s", w.Address())

	bal, deployed, err := w.Balance(ctx)
	require.NoError(t, err)
	assert.Equal(t, "0", bal.String(), "a fresh wallet is empty")
	assert.False(t, deployed)

	hist, err := w.History(ctx, 5)
	require.NoError(t, err)
	assert.Empty(t, hist)

	_, err = w.TokenBalance(ctx, USDT)
	assert.ErrorIs(t, err, ErrTokenUnavailable)
	t.Logf("testnet reachable, fresh wallet %s", w.Address())
}
