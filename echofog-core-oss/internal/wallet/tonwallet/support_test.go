package tonwallet

import (
	"math/big"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupportDefaultsToFivePercentWhenAddressSet(t *testing.T) {
	s := LoadSupport(t.TempDir(), "UQ-project")
	assert.True(t, s.Available())
	assert.Equal(t, 5, s.Percent())
}

func TestSupportInertWithoutAddress(t *testing.T) {
	s := LoadSupport(t.TempDir(), "")
	assert.False(t, s.Available())
	assert.Equal(t, 0, s.Percent())
	tip, err := s.Accrue("USDT", big.NewInt(10_000_000))
	require.NoError(t, err)
	assert.Zero(t, tip.Sign(), "no address: nothing is ever set aside")
	assert.Nil(t, s.Due("USDT"))
}

func TestSupportAccruesOnTopAndFlushesInBatches(t *testing.T) {
	s := LoadSupport(t.TempDir(), "UQ-project")

	// 5% of 5 USDT = 0.25 USDT: below the 1 USDT threshold, so nothing is sent yet
	tip, err := s.Accrue("USDT", big.NewInt(5_000_000))
	require.NoError(t, err)
	assert.Equal(t, int64(250_000), tip.Int64())
	assert.Nil(t, s.Due("USDT"))

	// three more payments push the pile over 1 USDT
	for i := 0; i < 3; i++ {
		_, err = s.Accrue("USDT", big.NewInt(5_000_000))
		require.NoError(t, err)
	}
	due := s.Due("USDT")
	require.NotNil(t, due)
	assert.Equal(t, int64(1_000_000), due.Int64())

	require.NoError(t, s.Settled("USDT", due))
	assert.Zero(t, s.Pending("USDT").Sign())
	assert.Nil(t, s.Due("USDT"))
}

func TestSupportAssetsAreSeparate(t *testing.T) {
	s := LoadSupport("", "UQ-project")
	_, _ = s.Accrue("TON", big.NewInt(1_000_000_000)) // 1 TON -> 0.05 TON
	assert.Equal(t, int64(50_000_000), s.Pending("TON").Int64())
	assert.Zero(t, s.Pending("USDT").Sign())
}

func TestSupportCanBeSwitchedOffAndPersists(t *testing.T) {
	dir := t.TempDir()
	s := LoadSupport(dir, "UQ-project")
	require.NoError(t, s.SetPercent(0))
	tip, _ := s.Accrue("USDT", big.NewInt(10_000_000))
	assert.Zero(t, tip.Sign())

	require.NoError(t, s.SetPercent(10))
	_, _ = s.Accrue("USDT", big.NewInt(10_000_000))

	again := LoadSupport(dir, "UQ-project")
	assert.Equal(t, 10, again.Percent(), "the choice survives a restart")
	assert.Equal(t, int64(1_000_000), again.Pending("USDT").Int64(), "so does the pile")
}

func TestSupportOffStaysOffAfterRestart(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, LoadSupport(dir, "UQ-project").SetPercent(0))
	assert.Equal(t, 0, LoadSupport(dir, "UQ-project").Percent(), "0 is a choice, not 'unset'")
}

func TestSupportRejectsAbsurdShare(t *testing.T) {
	s := LoadSupport("", "UQ-project")
	assert.ErrorIs(t, s.SetPercent(-1), ErrBadSupportPercent)
	assert.ErrorIs(t, s.SetPercent(21), ErrBadSupportPercent)
	assert.NoError(t, s.SetPercent(20))
}

func TestSupportSurvivesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, LoadSupport(dir, "UQ-project").SetPercent(7))
	// a damaged state must not crash or invent a debt
	require.NoError(t, writeFile(dir+"/support.json", "{not json"))
	s := LoadSupport(dir, "UQ-project")
	assert.Equal(t, DefaultSupportPercent, s.Percent())
	assert.Zero(t, s.Pending("USDT").Sign())
}

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }
