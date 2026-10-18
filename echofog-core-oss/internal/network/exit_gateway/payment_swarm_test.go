package exit_gateway

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole money cycle across two real nodes: the client learns the price and pay tag from the
// exit, is refused when the free test is used up, "pays" (the transfer appears in the exit's
// wallet history), gets served, and is cut off again when what it bought runs out.
func TestPaymentCycleBetweenTwoRealNodes(t *testing.T) {
	exitSwarm, exitTr := newSwarm(t)
	clientSwarm, clientTr := newSwarm(t)

	svc := NewExitService()
	svc.allowPrivate = true
	svc.SetEnabled(true)
	svc.SetPolicy(Policy{PricePerMB: price49, FreeBytes: 2000})
	ledger := NewLedger("")
	svc.SetPayments(ledger, exitSwarm.GetSelfID(), "UQ-exit-wallet")
	AttachToSwarm(exitSwarm, svc)

	require.NoError(t, clientTr.DialEarly(context.Background(), exitTr.Addr().String()))
	require.Eventually(t, func() bool {
		return len(exitSwarm.GetAllPeers()) > 0 && len(clientSwarm.GetAllPeers()) > 0
	}, 5*time.Second, 50*time.Millisecond)
	exitID := clientSwarm.GetAllPeers()[0].ID()
	dialer := NewP2PDialer(SwarmOpener(clientSwarm, exitID))
	ctx := context.Background()

	// 1. the client asks what it owes and where to pay
	info, err := dialer.QueryAccount(ctx)
	require.NoError(t, err)
	assert.True(t, info.CanPay())
	assert.Equal(t, "UQ-exit-wallet", info.PayAddress)
	assert.Equal(t, int64(price49), info.PricePerMB)
	assert.Equal(t, int64(2000), info.FreeLeft)
	assert.Equal(t, PayTag(clientSwarm.GetSelfID(), exitSwarm.GetSelfID()), info.Tag,
		"the tag the exit hands out is the one it will recognise, built from the authenticated IDs")

	// 2. a tunnel works while the free test lasts, then the quota is gone
	echo := echoServer(t)
	c, err := dialer.DialExitStream(ctx, echo)
	require.NoError(t, err)
	_, _ = c.Write(make([]byte, 900))
	_, err = io.ReadFull(c, make([]byte, 900))
	require.NoError(t, err)
	c.Close()
	require.Eventually(t, func() bool {
		i, err := dialer.QueryAccount(ctx)
		return err == nil && i.FreeLeft == 200
	}, 3*time.Second, 50*time.Millisecond)

	c, err = dialer.DialExitStream(ctx, echo)
	require.NoError(t, err)
	_, _ = c.Write(make([]byte, 600))
	_, err = io.ReadFull(c, make([]byte, 600))
	assert.Error(t, err, "cut when the free quota runs out")
	c.Close()
	_, err = dialer.DialExitStreamConfirmed(ctx, echo)
	assert.ErrorIs(t, err, ErrPaymentRequired)

	// 3. the client pays 0.01 USDT (the smallest credited amount) with the tag as the comment
	transferSeen := []IncomingPayment{{Hash: "real-tx-1", Comment: info.Tag, Micro: MinPayMicro, Time: time.Now()}}
	svc.ProcessPayments(transferSeen)

	after, err := dialer.QueryAccount(ctx)
	require.NoError(t, err)
	assert.Equal(t, info.BytesFor(MinPayMicro), after.Credit)
	assert.Greater(t, after.Credit, int64(100<<20), "0.01 USDT buys over 100 MB here")

	// 4. service resumes, and the same transfer seen again changes nothing
	svc.ProcessPayments(transferSeen)
	again, _ := dialer.QueryAccount(ctx)
	assert.Equal(t, after.Credit, again.Credit)

	c, err = dialer.DialExitStream(ctx, echo)
	require.NoError(t, err)
	msg := []byte("paid traffic through the swarm")
	_, err = c.Write(msg)
	require.NoError(t, err)
	got := make([]byte, len(msg))
	_, err = io.ReadFull(c, got)
	require.NoError(t, err)
	assert.Equal(t, msg, got)
	c.Close()

	// 5. UDP is charged against the same balance
	udp := udpEchoServer(t)
	u, err := dialer.DialExitUDP(ctx)
	require.NoError(t, err)
	defer u.Close()
	require.NoError(t, u.WriteTo([]byte("udp is paid too"), udp.LocalAddr().String()))
	b, _ := readWithin(t, u, 3*time.Second)
	assert.Equal(t, "udp is paid too", string(b))

	require.Eventually(t, func() bool {
		i, err := dialer.QueryAccount(ctx)
		return err == nil && i.Credit < after.Credit-int64(len(msg))
	}, 3*time.Second, 50*time.Millisecond, "the traffic was charged")

	// 6. another client's tag is separate: a stranger's payment does not feed this one
	stranger := PayTag(exitSwarm.GetSelfID(), clientSwarm.GetSelfID())
	svc.ProcessPayments([]IncomingPayment{{Hash: "stranger", Comment: stranger, Micro: MinPayMicro, Time: time.Now()}})
	last, _ := dialer.QueryAccount(ctx)
	assert.LessOrEqual(t, last.Credit, after.Credit, "credit for somebody else is not ours")
}
