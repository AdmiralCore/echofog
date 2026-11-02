package exit_gateway

import (
	"context"
	"testing"

	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// With a key the exit hands out a pay tag nobody can compute from the two peer IDs, in the old
// tag's shape so any client pays under it, and what a client paid under its old tag stays its own.
func TestKeyedPayTagIsUnguessableAndKeepsOldBalances(t *testing.T) {
	svc, _ := paidExit(t, 0)
	chain := newFakeChain()
	book := newTestBook(chain, "")
	svc.SetVouchers(book)
	legacy := PayTag(testClient, testExit)

	// paid before the exit had a key: a transfer and a vault voucher under the old tag
	require.Equal(t, 1, svc.ProcessPayments([]IncomingPayment{transfer("before", legacy, 1_000_000)}))
	u := newTestUser(t, 1, chain, 1_000_000_000)
	r := book.Accept(context.Background(), legacy, u.offer(t, tonwallet.MainnetGlobalID, exitPayout, 64*testNanoPerMB))
	require.True(t, r.OK, r.Error)
	paid := int64(1_000_000) * bytesPerMB / price49

	svc.SetPayTagKey([]byte("exit secret"))
	info := svc.AccountOf(testClient)
	assert.NotEqual(t, legacy, info.Tag, "the tag is no longer a hash of the two peer IDs")
	assert.Equal(t, KeyedPayTag([]byte("exit secret"), testClient), info.Tag)
	assert.Len(t, info.Tag, len(legacy), "same shape: clients copy it into the memo as before")
	assert.True(t, info.CanPay())
	assert.Equal(t, paid, info.Credit, "the transfer paid under the old tag moved over")
	require.NotNil(t, info.Vault)
	assert.Equal(t, int64(64<<20), info.Vault.Credit, "the vault bound to the old tag moved over")
	assert.Equal(t, info.Tag, svc.AccountOf(testClient).Tag, "stable")
	assert.NotEqual(t, info.Tag, svc.AccountOf(types.PeerID{2}).Tag, "per client")

	other := NewExitService()
	other.SetPayTagKey([]byte("another exit"))
	assert.NotEqual(t, info.Tag, KeyedPayTag([]byte("another exit"), testClient), "per exit key")

	// a client that read its old tag before the switch pays under it: still its money
	require.Equal(t, 1, svc.ProcessPayments([]IncomingPayment{transfer("late", legacy, 1_000_000)}))
	assert.Equal(t, 2*paid, svc.AccountOf(testClient).Credit)
	// and the new tag is credited like any tag
	require.Equal(t, 1, svc.ProcessPayments([]IncomingPayment{transfer("new", info.Tag, 1_000_000)}))
	assert.Equal(t, 3*paid, svc.AccountOf(testClient).Credit)
	assert.Equal(t, int64(64<<20)+3*paid+VoucherOverdraftBytes+OverdraftBytes, svc.allowance(testClient),
		"the tunnel sees both, with the overdraft of a client that has paid")
}
