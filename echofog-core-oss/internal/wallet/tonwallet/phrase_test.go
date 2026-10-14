package tonwallet

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xssnick/tonutils-go/ton/wallet"
)

func TestRecoveryPhraseIsDeterministicAndStandard(t *testing.T) {
	s := newSigner(t)
	p1, err := DeriveRecoveryPhrase(s)
	require.NoError(t, err)
	p2, err := DeriveRecoveryPhrase(s)
	require.NoError(t, err)
	assert.Equal(t, p1, p2, "the EchoFog key alone must restore the same TON phrase")
	assert.Len(t, p1, 24)

	// the reference TON implementation accepts it, so Tonkeeper/MyTonWallet can import it
	key, err := wallet.SeedToPrivateKeyWithOptions(p1, wallet.WithPassword(""), wallet.WithBIP39(false))
	require.NoError(t, err)

	ps, err := NewPhraseSigner(p1)
	require.NoError(t, err)
	pub, _ := ps.GetWalletPublicKey()
	assert.Equal(t, []byte(key.Public().(ed25519.PublicKey)), pub)
	sig, err := ps.SignWalletTransaction([]byte("msg"))
	require.NoError(t, err)
	assert.True(t, ed25519.Verify(pub, []byte("msg"), sig))

	// an app importing the phrase sees exactly the address EchoFog shows
	w, err := New(ps, Mainnet, "")
	require.NoError(t, err)
	ref, err := wallet.FromSeedWithOptions(nil, p1, versionConfig(Mainnet))
	require.NoError(t, err)
	assert.Equal(t, ref.WalletAddress().StringRaw(), w.RawAddress())

	other, err := DeriveRecoveryPhrase(newSigner(t))
	require.NoError(t, err)
	assert.NotEqual(t, strings.Join(p1, " "), strings.Join(other, " "))
}

func TestBasicSeedCheckMatchesReference(t *testing.T) {
	for i := 0; i < 20; i++ {
		assert.True(t, isBasicSeed(wallet.NewSeed()))
	}
	s := newSigner(t)
	secret, _ := s.SignWalletTransaction([]byte(phraseDomain))
	bad := 0
	for a := uint32(0); a < 64; a++ {
		c := candidate(secret, a, wordList())
		_, err := wallet.SeedToPrivateKeyWithOptions(c, wallet.WithPassword(""), wallet.WithBIP39(false))
		assert.Equal(t, err == nil, isBasicSeed(c))
		if err != nil {
			bad++
		}
	}
	assert.Greater(t, bad, 0)
}
