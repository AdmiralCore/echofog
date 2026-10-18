package exit_gateway

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTestFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }

var (
	testClient = types.PeerID{1}
	testExit   = types.PeerID{10}
)

// paidExit is an exit with a 1-MB-priced offer and a ledger, as StartSharing builds it.
func paidExit(t *testing.T, free int64) (*ExitService, *Ledger) {
	svc := NewExitService()
	svc.allowPrivate = true
	svc.SetEnabled(true)
	svc.SetPolicy(Policy{PricePerMB: price49, FreeBytes: free})
	l := NewLedger("")
	svc.SetPayments(l, testExit, "UQ-exit-wallet")
	return svc, l
}

func transfer(hash, comment string, micro int64) IncomingPayment {
	return IncomingPayment{Hash: hash, Comment: comment, Micro: micro, Time: time.Now()}
}

func TestAccountReportsWhereAndHowToPay(t *testing.T) {
	svc, _ := paidExit(t, 512*1024)
	info := svc.AccountOf(testClient)
	assert.True(t, info.CanPay())
	assert.Equal(t, "UQ-exit-wallet", info.PayAddress)
	assert.Equal(t, PayTag(testClient, testExit), info.Tag)
	assert.Equal(t, int64(512*1024), info.FreeLeft)
	assert.Equal(t, int64(price49), info.PricePerMB)
	assert.Zero(t, info.Credit)

	other := svc.AccountOf(types.PeerID{2})
	assert.NotEqual(t, info.Tag, other.Tag, "every client gets its own tag")
}

func TestAccountOfFreeOrUnpayableExit(t *testing.T) {
	free := NewExitService()
	free.SetEnabled(true)
	assert.False(t, free.AccountOf(testClient).CanPay(), "a free exit asks for nothing")

	noWallet := NewExitService()
	noWallet.SetEnabled(true)
	noWallet.SetPolicy(Policy{PricePerMB: price49})
	assert.False(t, noWallet.AccountOf(testClient).CanPay(), "priced but no wallet to pay into")
}

func TestProcessPaymentsCreditsTheRightTagOnce(t *testing.T) {
	svc, l := paidExit(t, 0)
	tag := PayTag(testClient, testExit)

	var got struct {
		sync.Mutex
		micro, bytes int64
	}
	svc.OnPayment(func(m, b int64) { got.Lock(); got.micro += m; got.bytes += b; got.Unlock() })

	list := []IncomingPayment{
		transfer("good", tag, 1_000_000),
		transfer("good", tag, 1_000_000),                   // same transfer in the same batch
		transfer("foreign", "thanks for lunch", 5_000_000), // somebody else's money
		transfer("dust", tag, 100),
		transfer("short", "ef1:abc", 1_000_000), // malformed tag
		{Hash: "old", Comment: tag, Micro: 1_000_000, Time: time.Now().Add(-ledgerKeep - time.Hour)},
	}
	assert.Equal(t, 1, svc.ProcessPayments(list))
	assert.Equal(t, 0, svc.ProcessPayments(list), "reading the history again credits nothing")

	credit, _ := l.Balance(tag)
	assert.Equal(t, int64(1_000_000)*bytesPerMB/price49, credit)
	got.Lock()
	assert.Equal(t, int64(1_000_000), got.micro)
	got.Unlock()
}

func TestProcessPaymentsDoesNothingOnAFreeExit(t *testing.T) {
	svc, l := paidExit(t, 0)
	svc.SetPolicy(Policy{})
	assert.Equal(t, 0, svc.ProcessPayments([]IncomingPayment{transfer("tx", PayTag(testClient, testExit), 1_000_000)}))
	assert.Zero(t, l.Earned())
}

func TestWatchPaymentsPollsAndBacksOff(t *testing.T) {
	svc, l := paidExit(t, 0)
	tag := PayTag(testClient, testExit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	calls := 0
	src := func(context.Context) ([]IncomingPayment, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return nil, io.ErrUnexpectedEOF // liteserver hiccup
		}
		return []IncomingPayment{transfer("tx1", tag, 1_000_000)}, nil
	}
	go svc.watchEvery(ctx, src, 20*time.Millisecond, 20*time.Millisecond)

	require.Eventually(t, func() bool { c, _ := l.Balance(tag); return c > 0 }, 2*time.Second, 10*time.Millisecond)
}

// A peer on the free list uses a priced exit without quota or payment; everybody else still pays.
func TestFreePeersUsePricedExitForNothing(t *testing.T) {
	svc, _ := paidExit(t, 1000)
	friend := types.PeerID{7}
	svc.SetFreePeers([]types.PeerID{friend})
	assert.Equal(t, 1, svc.FreePeers())

	assert.Equal(t, int64(-1), svc.allowance(friend))
	for i := 0; i < 5; i++ {
		require.True(t, svc.consume(friend, 10_000_000), "no quota for a free peer")
	}
	info := svc.AccountOf(friend)
	assert.Zero(t, info.PricePerMB, "a free peer is told the exit is free")
	assert.False(t, info.CanPay(), "and is never asked to pay")

	assert.Equal(t, int64(1000), svc.allowance(testClient), "others keep the normal offer")
	assert.False(t, svc.consume(testClient, 2000))
	assert.True(t, svc.AccountOf(testClient).CanPay())

	svc.SetFreePeers(nil)
	assert.Equal(t, int64(1000), svc.allowance(friend), "removed from the list: back to the normal offer")
}

// Two streams of one client share one allowance: the free quota cannot be multiplied.
func TestParallelStreamsShareOneAllowance(t *testing.T) {
	svc, _ := paidExit(t, 1000)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if svc.consume(testClient, 100) {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 10, ok, "1000 free bytes buy ten 100-byte reads however many streams ask")
	assert.Zero(t, svc.allowance(testClient))
}

func TestFreeQuotaIsSpentBeforePaidCredit(t *testing.T) {
	svc, l := paidExit(t, 1000)
	tag := PayTag(testClient, testExit)
	bought, ok := l.Credit(tag, "tx", MinPayMicro, price49)
	require.True(t, ok)

	require.True(t, svc.consume(testClient, 600))
	credit, _ := l.Balance(tag)
	assert.Equal(t, bought, credit, "paid credit untouched while free quota lasts")

	require.True(t, svc.consume(testClient, 600)) // 400 free + 200 paid
	credit, _ = l.Balance(tag)
	assert.Equal(t, bought-200, credit)
}

// The whole cycle on the exit side, byte by byte: free test, refusal, payment, service, cut-off.
func TestPaidTunnelLifecycle(t *testing.T) {
	target := echoServer(t)
	svc, l := paidExit(t, 1000)
	tag := PayTag(testClient, testExit)

	open := func(context.Context) (FramedStream, error) {
		client, server := newPipePair()
		go func() {
			if _, tgt, err := server.ReadFrame(); err == nil {
				svc.HandleTunnelFrom(context.Background(), server, tgt, testClient)
			}
		}()
		return client, nil
	}
	dial := func() (io.ReadWriteCloser, error) {
		return NewP2PDialer(open).DialExitStream(context.Background(), target)
	}

	// 1. the free test works and then runs dry
	c, err := dial()
	require.NoError(t, err)
	_, _ = c.Write(make([]byte, 400))
	_, err = io.ReadFull(c, make([]byte, 400))
	require.NoError(t, err)
	c.Close()
	require.Eventually(t, func() bool { return svc.allowance(testClient) == 200 }, 2*time.Second, 10*time.Millisecond)

	// 2. nothing left: refused
	c, err = dial()
	require.NoError(t, err)
	_, _ = c.Write(make([]byte, 600))
	_, err = io.ReadFull(c, make([]byte, 600))
	assert.Error(t, err, "the stream is cut when the free quota is gone")
	c.Close()
	_, err = NewP2PDialer(open).DialExitStreamConfirmed(context.Background(), target)
	assert.ErrorIs(t, err, ErrPaymentRequired)

	// 3. the client pays 0.10 USDT; the exit sees the transfer and credits it
	assert.Equal(t, 1, svc.ProcessPayments([]IncomingPayment{transfer("tx-pay", tag, 100_000)}))
	want := int64(100_000) * bytesPerMB / price49
	credit, _ := l.Balance(tag)
	assert.Equal(t, want, credit)

	// 4. service resumes and spends the credit
	c, err = dial()
	require.NoError(t, err)
	_, err = c.Write(make([]byte, 50_000))
	require.NoError(t, err)
	_, err = io.ReadFull(c, make([]byte, 50_000))
	require.NoError(t, err)
	c.Close()
	require.Eventually(t, func() bool {
		cr, _ := l.Balance(tag)
		return cr < want-90_000
	}, 2*time.Second, 10*time.Millisecond, "100 KB of traffic (up and down) was charged")
}
