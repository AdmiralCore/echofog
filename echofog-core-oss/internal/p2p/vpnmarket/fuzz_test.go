package vpnmarket

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

type fuzzSigner struct{ priv ed25519.PrivateKey }

func (s fuzzSigner) CreateSignature(d []byte) ([]byte, error) { return ed25519.Sign(s.priv, d), nil }

// FuzzDecodeAdvert: an exit advert is read from the DHT, written by anybody. Whatever the bytes,
// Decode must not panic, and what it accepts must verify again.
func FuzzDecodeAdvert(f *testing.F) {
	now := time.Unix(1_800_000_000, 0)
	priv := ed25519.NewKeyFromSeed(make([]byte, 32))
	a := fuzzAdvert(priv, now, 10, 1000, 64, "example.com")
	raw, _ := a.Encode()
	f.Add(raw)
	f.Add([]byte(`{"v":1}`))
	f.Add([]byte(`{"v":1,"pub":"AAAA","sig":"AAAA","issued_at":-9223372036854775808,"expires_at":9223372036854775807}`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		got, err := Decode(raw, now)
		if err != nil {
			return
		}
		if err := got.Verify(now); err != nil {
			t.Fatalf("accepted an advert that does not verify: %v", err)
		}
	})
}

// FuzzSignedAdvert signs adverts with values chosen by the fuzzer, as a hostile exit would: an
// accepted one must keep every bound the client relies on (price, speed, free test, the validity
// window, a printable SNI).
func FuzzSignedAdvert(f *testing.F) {
	f.Add(int64(10), 1000, 64, "example.com", int64(0), int64(600))
	f.Add(int64(-1), -1, -1, "", int64(-1<<62), int64(1<<62))
	f.Add(int64(MaxPricePerMB), MaxUploadKbps, MaxFreeTestKB, "a:b", int64(59), int64(60))
	f.Fuzz(func(t *testing.T, price int64, kbps, freeKB int, sni string, issuedOff, expiresOff int64) {
		now := time.Unix(1_800_000_000, 0)
		priv := ed25519.NewKeyFromSeed(make([]byte, 32))
		a := fuzzAdvert(priv, now, price, kbps, freeKB, sni)
		a.IssuedAt, a.ExpiresAt = now.Unix()+issuedOff, now.Unix()+expiresOff
		if err := a.Sign(fuzzSigner{priv}); err != nil {
			t.Fatal(err)
		}
		raw, err := a.Encode()
		if err != nil {
			return
		}
		got, err := Decode(raw, now)
		if err != nil {
			return
		}
		switch {
		case got.PricePerMB < 0 || got.PricePerMB > MaxPricePerMB, got.UploadKbps < 0 || got.UploadKbps > MaxUploadKbps,
			got.FreeTestKB < 0 || got.FreeTestKB > MaxFreeTestKB:
			t.Fatalf("value out of range accepted: %+v", got)
		case got.ExpiresAt <= now.Unix(), got.IssuedAt > now.Unix()+int64(maxClockSkew/time.Second),
			got.ExpiresAt-got.IssuedAt > int64(30*time.Minute/time.Second):
			t.Fatalf("bad validity window accepted: issued %d expires %d now %d", got.IssuedAt, got.ExpiresAt, now.Unix())
		}
		for i := 0; i < len(got.SNI); i++ {
			if c := got.SNI[i]; c < 0x21 || c > 0x7e {
				t.Fatalf("unprintable SNI accepted: %q", got.SNI)
			}
		}
	})
}

func fuzzAdvert(priv ed25519.PrivateKey, now time.Time, price int64, kbps, freeKB int, sni string) *Advert {
	pub := priv.Public().(ed25519.PublicKey)
	var pk types.PeerPublicKey
	copy(pk[:], pub)
	id := types.PeerPubKeyToID(pk)
	a := &Advert{
		Version: AdvertVersion, PeerID: hex.EncodeToString(id[:]), PubKey: pub, Enabled: true,
		PricePerMB: price, UploadKbps: kbps, FreeTestKB: freeKB, SNI: sni,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(AdvertTTL).Unix(),
	}
	_ = a.Sign(fuzzSigner{priv})
	return a
}
