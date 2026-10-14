package keymanager

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"strings"

	"github.com/awnumar/memguard"
	"github.com/tyler-smith/go-bip39"
	"golang.org/x/crypto/pbkdf2"
)

// GenerateSecureMnemonic produces a BIP-39 compliant 12-word mnemonic directly inside
// an mlocked, guarded memory buffer without allocating any immutable Go strings on the heap.
func GenerateSecureMnemonic() (*memguard.Enclave, error) {
	return GenerateMnemonicWords(12)
}

// GenerateMnemonicWords generates 12-word (128-bit) or 24-word (256-bit) BIP-39 mnemonics in RAM enclave.
func GenerateMnemonicWords(wordCount int) (*memguard.Enclave, error) {
	var entropyBytes int
	switch wordCount {
	case 12:
		entropyBytes = 16 // 128 bits
	case 24:
		entropyBytes = 32 // 256 bits
	default:
		return nil, errors.New("unsupported word count, only 12 or 24 allowed")
	}

	// 1. Allocate mlocked, guard-page protected buffer for entropy
	entropyBuf := memguard.NewBuffer(entropyBytes)
	defer entropyBuf.Destroy()

	if _, err := rand.Read(entropyBuf.Bytes()); err != nil {
		return nil, err
	}

	// 2. Compute SHA-256 checksum (CS = ENT / 32 bits)
	hash := sha256.Sum256(entropyBuf.Bytes())

	// For 128-bit: 128 + 4 = 132 bits -> 17 bytes (last byte contains checksum)
	// For 256-bit: 256 + 8 = 264 bits -> 33 bytes (last byte contains checksum)
	totalBytes := entropyBytes + 1
	dataBuf := memguard.NewBuffer(totalBytes)
	defer dataBuf.Destroy()

	copy(dataBuf.Bytes()[:entropyBytes], entropyBuf.Bytes())
	dataBuf.Bytes()[entropyBytes] = hash[0]
	memguard.WipeBytes(hash[:])

	// 3. Retrieve static BIP-39 wordlist (resides in binary rodata, not GC heap)
	wordList := bip39.GetWordList()

	// 4. Allocate protected buffer for mnemonic phrase
	phraseBuf := memguard.NewBuffer(wordCount * 12)
	defer phraseBuf.Destroy()

	data := dataBuf.Bytes()
	pos := 0

	// 5. Extract words from 11-bit chunks directly as bytes into protected buffer
	for w := 0; w < wordCount; w++ {
		startBit := w * 11
		byteIdx := startBit / 8
		bitOffset := startBit % 8

		val := (uint32(data[byteIdx]) << 16) | (uint32(data[byteIdx+1]) << 8)
		if byteIdx+2 < len(data) {
			val |= uint32(data[byteIdx+2])
		}

		shift := 24 - (bitOffset + 11)
		wordIdx := int((val >> shift) & 0x7FF)

		word := wordList[wordIdx]
		if w > 0 {
			phraseBuf.Bytes()[pos] = ' '
			pos++
		}
		copy(phraseBuf.Bytes()[pos:], word)
		pos += len(word)
	}

	// 6. Create exact sized locked enclave
	exactBuf := memguard.NewBufferFromBytes(phraseBuf.Bytes()[:pos])
	defer exactBuf.Destroy()

	return exactBuf.Seal(), nil
}

// SeedFromMnemonic derives a 64-byte binary seed from mnemonic and passphrase
// using PBKDF2-HMAC-SHA512 (2048 iterations) with guaranteed zeroize cleanup.
func SeedFromMnemonic(mnemonic []byte, passphrase []byte) (*memguard.Enclave, error) {
	if len(mnemonic) == 0 {
		return nil, errors.New("mnemonic is empty")
	}

	// Salt is []byte("mnemonic") + passphrase
	prefix := []byte("mnemonic")
	saltBuf := memguard.NewBuffer(len(prefix) + len(passphrase))
	defer saltBuf.Destroy()

	copy(saltBuf.Bytes(), prefix)
	if len(passphrase) > 0 {
		copy(saltBuf.Bytes()[len(prefix):], passphrase)
	}

	// 64-byte seed output via PBKDF2
	derived := pbkdf2.Key(mnemonic, saltBuf.Bytes(), 2048, 64, sha512.New)
	defer memguard.WipeBytes(derived)

	seedBuf := memguard.NewBufferFromBytes(derived)
	defer seedBuf.Destroy()

	return seedBuf.Seal(), nil
}

// CheckPhrase normalises a recovery phrase someone typed or pasted (case, spaces, line breaks) and
// says what is wrong with it in words: the count, a word not in the list, or the checksum. A typo
// caught here does not turn into a different identity.
func CheckPhrase(phrase string) (string, error) {
	words := strings.Fields(strings.ToLower(phrase))
	if n := len(words); n != 12 && n != 24 {
		return "", fmt.Errorf("a recovery phrase has 12 or 24 words, this one has %d", n)
	}
	for i, w := range words {
		if _, ok := bip39.GetWordIndex(w); !ok {
			return "", fmt.Errorf("word %d (%q) is not in the recovery word list: check its spelling", i+1, w)
		}
	}
	norm := strings.Join(words, " ")
	if !bip39.IsMnemonicValid(norm) {
		return "", errors.New("the words are right but their checksum is not: a word is missing, swapped or mistyped")
	}
	return norm, nil
}
