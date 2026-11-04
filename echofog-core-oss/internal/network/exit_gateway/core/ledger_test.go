package exit_gateway

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const price49 = 49 // micro-USDT per MB, about 5 cents a GB

func TestPayTagIsStableAndPairSpecific(t *testing.T) {
	var a, b, exit1, exit2 types.PeerID
	a[0], b[0], exit1[0], exit2[0] = 1, 2, 10, 11
	assert.Equal(t, PayTag(a, exit1), PayTag(a, exit1))
	assert.NotEqual(t, PayTag(a, exit1), PayTag(b, exit1), "another client")
	assert.NotEqual(t, PayTag(a, exit1), PayTag(a, exit2), "another exit")
	assert.Regexp(t, `^ef1:[0-9a-f]{16}$`, PayTag(a, exit1))
}

func TestLedgerCreditsBytesAtThePrice(t *testing.T) {
	l := NewLedger("")
	bytes, ok := l.Credit("ef1:aaaaaaaaaaaaaaaa", "tx1", 1_000_000, price49) // 1 USDT
	require.True(t, ok)
	assert.Equal(t, int64(1_000_000)*bytesPerMB/price49, bytes)
	assert.InDelta(t, 20_408, float64(bytes)/bytesPerMB, 1, "1 USDT at ~5c/GB is ~20 GB")

	credit, paid := l.Balance("ef1:aaaaaaaaaaaaaaaa")
	assert.Equal(t, bytes, credit)
	assert.True(t, paid)
	assert.Equal(t, int64(1_000_000), l.Earned())
}

func TestLedgerNeverCreditsATransferTwice(t *testing.T) {
	l := NewLedger("")
	_, ok := l.Credit("ef1:aaaaaaaaaaaaaaaa", "tx1", 1_000_000, price49)
	require.True(t, ok)
	_, ok = l.Credit("ef1:aaaaaaaaaaaaaaaa", "tx1", 1_000_000, price49)
	assert.False(t, ok, "the same transfer read again from the history")
	credit, _ := l.Balance("ef1:aaaaaaaaaaaaaaaa")
	assert.Equal(t, int64(1_000_000)*bytesPerMB/price49, credit)
	assert.True(t, l.Seen("tx1", time.Now()))
}

func TestLedgerIgnoresDustAndNonsense(t *testing.T) {
	l := NewLedger("")
	for name, c := range map[string]struct {
		tag, hash string
		micro, p  int64
	}{
		"dust":      {"ef1:aaaaaaaaaaaaaaaa", "t1", MinPayMicro - 1, price49},
		"no tag":    {"", "t2", 1_000_000, price49},
		"no hash":   {"ef1:aaaaaaaaaaaaaaaa", "", 1_000_000, price49},
		"free exit": {"ef1:aaaaaaaaaaaaaaaa", "t3", 1_000_000, 0},
	} {
		_, ok := l.Credit(c.tag, c.hash, c.micro, c.p)
		assert.False(t, ok, name)
	}
	assert.Zero(t, l.Earned())
}

func TestLedgerConsumeAndOverdraft(t *testing.T) {
	l := NewLedger("")
	tag := "ef1:aaaaaaaaaaaaaaaa"

	assert.False(t, l.Consume(tag, 1), "a stranger has no credit and no overdraft")

	// a paying client: the smallest accepted transfer buys about 204 MB
	bought, ok := l.Credit(tag, "tx1", MinPayMicro, price49)
	require.True(t, ok)
	require.Greater(t, bought, int64(100*bytesPerMB))

	assert.True(t, l.Consume(tag, bought), "spend it all")
	assert.True(t, l.Consume(tag, OverdraftBytes), "a client that paid before may overdraw")
	assert.False(t, l.Consume(tag, 1), "but only up to the limit")
	assert.Zero(t, l.Available(tag))

	// the next payment first repays the overdraft
	more, ok := l.Credit(tag, "tx2", 10*MinPayMicro, price49)
	require.True(t, ok)
	credit, _ := l.Balance(tag)
	assert.Equal(t, more-OverdraftBytes, credit)
}

func TestLedgerPersistsAndForgetsOldTransfers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l := NewLedger(path)
	tag := "ef1:aaaaaaaaaaaaaaaa"
	_, ok := l.Credit(tag, "tx1", 1_000_000, price49)
	require.True(t, ok)
	require.True(t, l.Consume(tag, 5*bytesPerMB))
	require.NoError(t, l.Flush())

	again := NewLedger(path)
	credit, paid := again.Balance(tag)
	assert.True(t, paid)
	assert.Equal(t, int64(1_000_000)*bytesPerMB/price49-5*bytesPerMB, credit, "credit and spending both survive a restart")
	_, ok = again.Credit(tag, "tx1", 1_000_000, price49)
	assert.False(t, ok, "and so does the memory of which transfers were booked")

	assert.True(t, again.Seen("ancient", time.Now().Add(-ledgerKeep-time.Hour)), "too old to be considered")
}

func TestLedgerSurvivesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	require.NoError(t, writeTestFile(path, "{broken"))
	l := NewLedger(path)
	assert.Zero(t, l.Earned())
	_, ok := l.Credit("ef1:aaaaaaaaaaaaaaaa", "tx", 1_000_000, price49)
	assert.True(t, ok)
}

func TestMulDivDoesNotOverflow(t *testing.T) {
	assert.Equal(t, int64(0), mulDiv(5, 5, 0))
	assert.Equal(t, int64(2_000_000_000_000), mulDiv(1_000_000_000_000, 2, 1))
	// 1M USDT at 1 micro/MB: 10^12 * 2^20 / 1 would overflow a naive a*b
	assert.Equal(t, int64(1_000_000_000_000)*bytesPerMB, mulDiv(1_000_000_000_000, bytesPerMB, 1))
}
