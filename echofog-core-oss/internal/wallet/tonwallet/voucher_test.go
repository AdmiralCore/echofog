package tonwallet

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// voucherFixture is written by contracts/vault/scripts/makeFixture.ts. The contract tests check
// that these exact bytes are accepted by the contract; the tests here check that Go builds the
// same bytes. Together they prove the Go client and node speak the contract's format.
type voucherFixture struct {
	SeedHex       string `json:"seed_hex"`
	PubkeyHex     string `json:"pubkey_hex"`
	Owner         string `json:"owner"`
	Node          string `json:"node"`
	GlobalID      int32  `json:"global_id"`
	Cumulative    string `json:"cumulative"`
	CodeHashHex   string `json:"code_hash_hex"`
	DataHashHex   string `json:"data_hash_hex"`
	VaultRaw      string `json:"vault_raw"`
	VoucherBocHex string `json:"voucher_boc_hex"`
	VoucherHash   string `json:"voucher_hash_hex"`
	SignatureHex  string `json:"signature_hex"`
}

func loadFixture(t *testing.T) voucherFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/voucher_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx voucherFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureVoucher(t *testing.T, fx voucherFixture) (Voucher, ed25519.PrivateKey) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(mustHex(t, fx.SeedHex))
	owner := address.MustParseRawAddr(fx.Owner)
	vault, err := VaultAddress(owner, priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	cum, _ := new(big.Int).SetString(fx.Cumulative, 10)
	return Voucher{GlobalID: fx.GlobalID, Vault: vault, Node: address.MustParseRawAddr(fx.Node), Cumulative: cum}, priv
}

func TestVaultCodeIsTheContractBuild(t *testing.T) {
	fx := loadFixture(t)
	code, err := VaultCode()
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(code.Hash()); got != fx.CodeHashHex {
		t.Fatalf("embedded vault code %s, contract build %s: run `npm run build` in contracts/vault", got, fx.CodeHashHex)
	}
}

func TestVaultAddressMatchesContract(t *testing.T) {
	fx := loadFixture(t)
	priv := ed25519.NewKeyFromSeed(mustHex(t, fx.SeedHex))
	pub := priv.Public().(ed25519.PublicKey)
	if hex.EncodeToString(pub) != fx.PubkeyHex {
		t.Fatal("ed25519 seed gives a different key in Go and in TypeScript")
	}
	owner := address.MustParseRawAddr(fx.Owner)
	si, err := VaultStateInit(owner, pub)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(si.Data.Hash()); got != fx.DataHashHex {
		t.Fatalf("initial data hash %s, want %s", got, fx.DataHashHex)
	}
	vault, err := VaultAddress(owner, pub)
	if err != nil {
		t.Fatal(err)
	}
	if vault.StringRaw() != fx.VaultRaw {
		t.Fatalf("vault address %s, want %s", vault.StringRaw(), fx.VaultRaw)
	}
	if !vault.IsBounceable() {
		t.Fatal("a deployed vault should be addressed bounceable")
	}
}

func TestVoucherBytesAndSignatureMatchContract(t *testing.T) {
	fx := loadFixture(t)
	v, priv := fixtureVoucher(t, fx)

	c, err := v.Cell()
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(c.Hash()); got != fx.VoucherHash {
		t.Fatalf("voucher hash %s, want %s", got, fx.VoucherHash)
	}
	tsCell, err := cell.FromBOC(mustHex(t, fx.VoucherBocHex))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tsCell.Hash(), c.Hash()) {
		t.Fatal("the TypeScript voucher cell differs from the Go one")
	}

	sv, err := SignVoucherWith(keySigner{priv}, v)
	if err != nil {
		t.Fatal(err)
	}
	// ed25519 is deterministic: the same key and hash give the same signature in both languages,
	// and the contract tests accept exactly this one.
	if got := hex.EncodeToString(sv.Signature); got != fx.SignatureHex {
		t.Fatalf("signature %s, want %s", got, fx.SignatureHex)
	}
	if !VerifyVoucher(priv.Public().(ed25519.PublicKey), v, sv.Signature) {
		t.Fatal("own signature does not verify")
	}
}

func TestVoucherVerifyRejectsChanges(t *testing.T) {
	fx := loadFixture(t)
	v, priv := fixtureVoucher(t, fx)
	pub := priv.Public().(ed25519.PublicKey)
	sv, err := SignVoucherWith(keySigner{priv}, v)
	if err != nil {
		t.Fatal(err)
	}

	more := v
	more.Cumulative = new(big.Int).Add(v.Cumulative, big.NewInt(1))
	other := v
	other.Node = address.MustParseRawAddr("0:" + hex.EncodeToString(bytes.Repeat([]byte{0x33}, 32)))
	testnet := v
	testnet.GlobalID = TestnetGlobalID
	for name, changed := range map[string]Voucher{"amount": more, "node": other, "network": testnet} {
		if VerifyVoucher(pub, changed, sv.Signature) {
			t.Fatalf("a voucher with a changed %s still verifies", name)
		}
	}
	stranger := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)).Public().(ed25519.PublicKey)
	if VerifyVoucher(stranger, v, sv.Signature) {
		t.Fatal("verifies under another key")
	}
	if VerifyVoucher(pub, v, sv.Signature[:63]) {
		t.Fatal("verifies with a short signature")
	}
}

func TestVoucherOutOfRangeIsAnErrorNotAPanic(t *testing.T) {
	fx := loadFixture(t)
	v, _ := fixtureVoucher(t, fx)
	v.Cumulative = new(big.Int).Lsh(big.NewInt(1), 130)
	if _, err := v.Cell(); err == nil {
		t.Fatal("a 130-bit amount cannot be a voucher")
	}
	v.Cumulative = big.NewInt(-1)
	if _, err := v.Cell(); err == nil {
		t.Fatal("a negative amount cannot be a voucher")
	}
	v.Cumulative, v.Node = big.NewInt(1), nil
	if _, err := v.Cell(); err == nil {
		t.Fatal("a voucher needs a node")
	}
}

func TestClaimBodyLayout(t *testing.T) {
	fx := loadFixture(t)
	v, priv := fixtureVoucher(t, fx)
	sv, err := SignVoucherWith(keySigner{priv}, v)
	if err != nil {
		t.Fatal(err)
	}
	body, err := ClaimBody(sv, 42)
	if err != nil {
		t.Fatal(err)
	}
	s := body.MustBeginParse()
	if op := s.MustLoadUInt(32); op != opVaultClaim {
		t.Fatalf("op %x", op)
	}
	if q := s.MustLoadUInt(64); q != 42 {
		t.Fatalf("query id %d", q)
	}
	ref := s.MustLoadRef()
	vc, _ := v.Cell()
	if !bytes.Equal(ref.MustToCell().Hash(), vc.Hash()) {
		t.Fatal("claim does not carry the voucher")
	}
	if sig := s.MustLoadSlice(512); !bytes.Equal(sig, sv.Signature) {
		t.Fatal("claim does not carry the signature")
	}
	if s.BitsLeft() != 0 || s.RefsNum() != 0 {
		t.Fatal("claim body has trailing data")
	}
}

func TestWalletSignsVouchersForItsOwnVault(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, 32))
	w, err := New(keySigner{priv}, Testnet, "")
	if err != nil {
		t.Fatal(err)
	}
	node := address.MustParseRawAddr("0:" + hex.EncodeToString(bytes.Repeat([]byte{0x44}, 32)))
	sv, err := w.SignVoucher(node, big.NewInt(3906))
	if err != nil {
		t.Fatal(err)
	}
	own, _ := w.Vault()
	if sv.GlobalID != TestnetGlobalID || !sv.Vault.Equals(own) {
		t.Fatal("voucher is not for this wallet's testnet vault")
	}
	if !VerifyVoucher(w.PublicKey(), sv.Voucher, sv.Signature) {
		t.Fatal("does not verify")
	}
	w.SetFrozen(true)
	if _, err := w.SignVoucher(node, big.NewInt(1)); err != ErrFrozen {
		t.Fatalf("frozen wallet signed a voucher: %v", err)
	}
}

func TestParseVaultState(t *testing.T) {
	owner := cell.BeginCell().MustStoreAddr(address.MustParseRawAddr("0:" + hex.EncodeToString(bytes.Repeat([]byte{1}, 32)))).EndCell().MustBeginParse()
	at := time.Unix(1_800_000_000, 0)
	info, err := parseVaultState([]any{owner, big.NewInt(7), big.NewInt(5_000_000), big.NewInt(at.Unix()), big.NewInt(at.Unix() - 60)})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Deployed || info.Deposit.Int64() != 5_000_000 || !info.WithdrawAt.Equal(at) || !info.Closing() {
		t.Fatalf("parsed %+v", info)
	}
	info, err = parseVaultState([]any{owner, big.NewInt(7), big.NewInt(1), big.NewInt(0), big.NewInt(1)})
	if err != nil || info.Closing() {
		t.Fatalf("withdrawAt 0 must mean no withdrawal: %+v %v", info, err)
	}
	if _, err := parseVaultState([]any{owner}); err == nil {
		t.Fatal("short getter result accepted")
	}
}
