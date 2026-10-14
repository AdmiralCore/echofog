package tonwallet

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/awnumar/memguard"
	"github.com/tyler-smith/go-bip39"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"golang.org/x/crypto/pbkdf2"
)

// phraseDomain separates this derivation from every other use of the wallet key.
const phraseDomain = "echofog/ton-recovery-phrase/v1"

const phraseWords = 24

// DeriveRecoveryPhrase returns the standard 24-word TON phrase of this EchoFog identity.
//
// It is derived deterministically from the EchoFog wallet key (an ed25519 signature over a
// fixed domain string is a stable secret only the key holder can produce), so the EchoFog
// mnemonic alone restores the TON wallet, while the 24 words also import into Tonkeeper,
// MyTonWallet or any other TON wallet.
func DeriveRecoveryPhrase(s Signer) ([]string, error) {
	secret, err := s.SignWalletTransaction([]byte(phraseDomain))
	if err != nil {
		return nil, fmt.Errorf("derive TON phrase: %w", err)
	}
	defer memguard.WipeBytes(secret)

	list := wordList()
	// TON accepts a phrase only if its PBKDF2 check byte is zero (1 in 256): try candidates
	// in a fixed order so the result is the same on every device.
	for attempt := uint32(0); attempt < 1<<16; attempt++ {
		phrase := candidate(secret, attempt, list)
		if isBasicSeed(phrase) {
			return phrase, nil
		}
	}
	return nil, errors.New("derive TON phrase: no valid candidate")
}

func candidate(secret []byte, attempt uint32, list []string) []string {
	mac := hmac.New(sha512.New, secret)
	var ctr [4]byte
	binary.BigEndian.PutUint32(ctr[:], attempt)
	mac.Write(ctr[:])
	stream := mac.Sum(nil) // 512 bits, 24 words need 264

	out := make([]string, phraseWords)
	for i := 0; i < phraseWords; i++ {
		bit := i * 11
		v := uint32(stream[bit/8])<<16 | uint32(stream[bit/8+1])<<8 | uint32(stream[bit/8+2])
		idx := (v >> (24 - 11 - uint(bit%8))) & 0x7ff
		out[i] = list[idx]
	}
	return out
}

// isBasicSeed mirrors the TON reference check for phrases without a password.
func isBasicSeed(phrase []string) bool {
	mac := hmac.New(sha512.New, []byte(strings.Join(phrase, " ")))
	entropy := mac.Sum(nil)
	p := pbkdf2.Key(entropy, []byte("TON seed version"), 100000/256, 1, sha512.New)
	return p[0] == 0
}

// PhraseSigner signs with the key of a TON phrase. The private key lives in a memguard
// enclave and is only unsealed for the duration of a signature.
type PhraseSigner struct {
	enclave *memguard.Enclave
	pub     ed25519.PublicKey
}

// NewPhraseSigner derives the TON wallet key the same way TON wallet apps do.
func NewPhraseSigner(phrase []string) (*PhraseSigner, error) {
	key, err := wallet.SeedToPrivateKeyWithOptions(phrase, wallet.WithPassword(""), wallet.WithBIP39(false))
	if err != nil {
		return nil, err
	}
	pub := append(ed25519.PublicKey(nil), key.Public().(ed25519.PublicKey)...)
	seed := append([]byte(nil), key.Seed()...)
	memguard.WipeBytes(key)
	return &PhraseSigner{enclave: memguard.NewEnclave(seed), pub: pub}, nil
}

func (p *PhraseSigner) SignWalletTransaction(data []byte) ([]byte, error) {
	buf, err := p.enclave.Open()
	if err != nil {
		return nil, fmt.Errorf("open TON key: %w", err)
	}
	defer buf.Destroy()
	priv := ed25519.NewKeyFromSeed(buf.Bytes())
	defer memguard.WipeBytes(priv)
	return ed25519.Sign(priv, data), nil
}

func (p *PhraseSigner) GetWalletPublicKey() ([]byte, error) {
	return append([]byte(nil), p.pub...), nil
}

func wordList() []string { return bip39.GetWordList() }
