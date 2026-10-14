package tonwallet

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton/jetton"
	"github.com/xssnick/tonutils-go/ton/wallet"
)

func TestUnitsUSDT(t *testing.T) {
	n, err := ParseUnits("12.5", 6)
	require.NoError(t, err)
	assert.Equal(t, "12500000", n.String())
	_, err = ParseUnits("0.0000001", 6)
	assert.ErrorIs(t, err, ErrBadAmount)
	assert.Equal(t, "0.000001", FormatUnits(big.NewInt(1), 6))
	assert.Equal(t, "3", FormatUnits(big.NewInt(3_000_000), 6))
}

func TestBuildTokenTransfer(t *testing.T) {
	jw := address.MustParseAddr("EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs")
	recipient, err := New(newSigner(t), Mainnet, "")
	require.NoError(t, err)
	to := address.MustParseAddr(recipient.Address())
	owner := address.MustParseAddr("EQD4vUD2PYRLQd0mSwjmnnWSpeulTjZoFypJVUJAyJoUbrRu")

	msg, err := buildTokenTransfer(jw, to, owner, big.NewInt(2_500_000), "proxy 4.2 GB")
	require.NoError(t, err)
	im := msg.InternalMessage
	assert.Equal(t, jw.StringRaw(), im.DstAddr.StringRaw(), "the request goes to OUR jetton wallet, not the recipient")
	assert.Equal(t, JettonGas.String(), im.Amount.Nano().String())
	assert.True(t, im.Bounce, "if the jetton wallet rejects it, the TON comes back")

	var p jetton.TransferPayload
	require.NoError(t, loadCell(&p, im.Body))
	assert.Equal(t, "2500000", p.Amount.Nano().String())
	assert.Equal(t, to.StringRaw(), p.Destination.StringRaw())
	assert.Equal(t, owner.StringRaw(), p.ResponseDestination.StringRaw(), "excess gas returns to us")
	assert.Equal(t, "1", p.ForwardTONAmount.Nano().String(), "recipient wallet gets a notification")
	assert.Equal(t, "proxy 4.2 GB", commentOf(p.ForwardPayload))
}

func notification(t *testing.T, from *address.Address, amount int64, comment string) *tlb.Transaction {
	t.Helper()
	fwd, err := wallet.CreateCommentCell(comment)
	require.NoError(t, err)
	body, err := tlb.ToCell(jetton.TransferNotification{QueryID: 1, Amount: tlb.FromNanoTONU(uint64(amount)), Sender: address.MustParseAddr("EQD4vUD2PYRLQd0mSwjmnnWSpeulTjZoFypJVUJAyJoUbrRu"), ForwardPayload: fwd})
	require.NoError(t, err)
	tx := &tlb.Transaction{Hash: []byte{1}, LT: 1, Now: uint32(time.Now().Unix())}
	tx.IO.In = &tlb.Message{MsgType: tlb.MsgTypeInternal, Msg: &tlb.InternalMessage{SrcAddr: from, DstAddr: from, Amount: tlb.FromNanoTONU(1), Body: body}}
	return tx
}

func TestIncomingTokenOnlyFromOurJettonWallet(t *testing.T) {
	ours := address.MustParseAddr("EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs")
	fake := address.MustParseAddr("EQD4vUD2PYRLQd0mSwjmnnWSpeulTjZoFypJVUJAyJoUbrRu")

	tx := toTx(notification(t, ours, 5_000_000, "top-up"), Mainnet, ours.StringRaw())
	assert.Equal(t, "USDT", tx.Asset)
	assert.True(t, tx.Incoming)
	assert.Equal(t, "5000000", tx.AmountNano)
	assert.Equal(t, "top-up", tx.Comment)

	spoof := toTx(notification(t, fake, 1_000_000_000, "free money"), Mainnet, ours.StringRaw())
	assert.Equal(t, "TON", spoof.Asset, "a look-alike notification from another contract is not USDT")
}

// TestLiveMainnetUSDT reads real USDT contracts (read-only). Run with TON_LIVE=1.
func TestLiveMainnetUSDT(t *testing.T) {
	if os.Getenv("TON_LIVE") == "" {
		t.Skip("set TON_LIVE=1 to query TON mainnet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	w, err := New(newSigner(t), Mainnet, t.TempDir())
	require.NoError(t, err)

	jw, err := w.TokenWalletAddress(ctx, USDT)
	require.NoError(t, err)
	assert.NotEqual(t, w.RawAddress(), jw.StringRaw())
	bal, err := w.TokenBalance(ctx, USDT)
	require.NoError(t, err)
	assert.Equal(t, int64(0), bal.Int64(), "a fresh owner holds no USDT")

	// a real holder exercises the active jetton wallet path
	holder := topUSDTHolder(t, ctx)
	owner, err := w.ParseRecipient(holder) // tonapi returns raw 0:… form
	require.NoError(t, err)
	hb, err := w.tokenBalanceOf(ctx, USDT, owner)
	require.NoError(t, err)
	assert.Positive(t, hb.Sign())
	t.Logf("holder %s has %s USDT", holder, FormatUnits(hb, USDT.Decimals))

	tw, err := New(newSigner(t), Testnet, "")
	require.NoError(t, err)
	_, err = tw.TokenBalance(ctx, USDT)
	assert.ErrorIs(t, err, ErrTokenUnavailable)
}

func topUSDTHolder(t *testing.T, ctx context.Context) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://tonapi.io/v2/jettons/"+USDT.MasterMainnet+"/holders?limit=1", nil)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	var body struct {
		Addresses []struct {
			Owner struct {
				Address string `json:"address"`
			} `json:"owner"`
		} `json:"addresses"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.NotEmpty(t, body.Addresses)
	return body.Addresses[0].Owner.Address
}
