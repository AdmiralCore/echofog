package keymanager

import (
	"bytes"
	"strings"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/tyler-smith/go-bip39"
)

func TestGenerateSecureMnemonic_Validity(t *testing.T) {
	defer memguard.Purge()

	// 1. Test 12-word default generation
	enclave12, err := GenerateSecureMnemonic()
	if err != nil {
		t.Fatalf("GenerateSecureMnemonic (12 words) failed: %v", err)
	}

	locked12, err := enclave12.Open()
	if err != nil {
		t.Fatalf("failed to open enclave: %v", err)
	}
	defer locked12.Destroy()

	phraseBytes12 := locked12.Bytes()
	words12 := strings.Split(string(phraseBytes12), " ")
	if len(words12) != 12 {
		t.Fatalf("expected 12 words, got %d: %s", len(words12), string(phraseBytes12))
	}

	if !bip39.IsMnemonicValid(string(phraseBytes12)) {
		t.Fatalf("12-word mnemonic failed bip39 validation: %s", string(phraseBytes12))
	}

	// 2. Test 24-word cold storage generation
	enclave24, err := GenerateMnemonicWords(24)
	if err != nil {
		t.Fatalf("GenerateMnemonicWords(24) failed: %v", err)
	}

	locked24, err := enclave24.Open()
	if err != nil {
		t.Fatalf("failed to open 24-word enclave: %v", err)
	}
	defer locked24.Destroy()

	phraseBytes24 := locked24.Bytes()
	words24 := strings.Split(string(phraseBytes24), " ")
	if len(words24) != 24 {
		t.Fatalf("expected 24 words, got %d", len(words24))
	}

	if !bip39.IsMnemonicValid(string(phraseBytes24)) {
		t.Fatalf("24-word mnemonic failed bip39 validation: %s", string(phraseBytes24))
	}

	// 3. Test unsupported count
	_, err = GenerateMnemonicWords(15)
	if err == nil {
		t.Fatalf("expected error for 15 words, got nil")
	}
}

func TestSeedFromMnemonic_Compatibility(t *testing.T) {
	defer memguard.Purge()

	testMnemonic := []byte("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	testPass := []byte("TREZOR")

	// 1. Generate seed using our pure-go memguard implementation
	seedEnclave, err := SeedFromMnemonic(testMnemonic, testPass)
	if err != nil {
		t.Fatalf("SeedFromMnemonic failed: %v", err)
	}

	lockedSeed, err := seedEnclave.Open()
	if err != nil {
		t.Fatalf("failed to open seed enclave: %v", err)
	}
	defer lockedSeed.Destroy()

	// 2. Generate expected seed using standard go-bip39
	expectedSeed := bip39.NewSeed(string(testMnemonic), string(testPass))

	if !bytes.Equal(lockedSeed.Bytes(), expectedSeed) {
		t.Fatalf("seed mismatch! got %x, expected %x", lockedSeed.Bytes(), expectedSeed)
	}
}

func TestCheckPhrase(t *testing.T) {
	ok := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	got, err := CheckPhrase("  ABANDON abandon\nabandon abandon abandon abandon abandon abandon abandon abandon abandon   About \r\n")
	if err != nil || got != ok {
		t.Fatalf("a pasted phrase was not normalised: %q %v", got, err)
	}
	for phrase, want := range map[string]string{
		"abandon abandon": "12 or 24 words",
		"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abuot":   "word 12",
		"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon": "checksum",
	} {
		if _, err := CheckPhrase(phrase); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: want %q, got %v", phrase, want, err)
		}
	}
}
