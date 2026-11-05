package packet

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/curve25519"

	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/elligator"
)

// chiSquareP01 is the 0.99 quantile of the chi-square distribution with 255 degrees of freedom:
// a byte stream passes when its statistic is at most this value (p-value >= 0.01).
const chiSquareP01 = 310.457

func chiSquareBytes(data []byte) float64 {
	var counts [256]float64
	for _, b := range data {
		counts[b]++
	}
	n := float64(len(data))
	expected := n / 256
	stat := 0.0
	for _, c := range counts {
		d := c - expected
		stat += d * d / expected
	}
	return stat
}

func TestByteDistribution(t *testing.T) {
	// A fixed reader keeps the streams deterministic: the first run either passes or it does
	// not. The keys still come from the normal derivation path.
	rnd := rand.New(rand.NewSource(1))
	fixed := func(label string) []byte {
		out := make([]byte, 32)
		for i := range out {
			out[i] = byte(i*7 + len(label))
		}
		return out
	}
	maskSK := fixed("mask secret")
	maskPublic, err := curve25519.X25519(maskSK, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	u := fixed("u")
	repr := fixed("repr")
	keys, err := Derive(fixed("z"), maskPublic, u, repr)
	if err != nil {
		t.Fatal(err)
	}

	const packets = 800
	const length = 1280
	initBytes := make([]byte, 0, packets*length)
	inner := make([]byte, 1200)
	for i := range inner {
		inner[i] = byte(i)
	}
	for i := 0; i < packets; i++ {
		sk, rep, err := elligator.Generate(rnd)
		if err != nil {
			t.Fatal(err)
		}
		z, err := curve25519.X25519(sk, maskPublic)
		if err != nil {
			t.Fatal(err)
		}
		ue := elligator.Forward(rep)
		initKeys, err := Derive(z, maskPublic, ue, rep)
		if err != nil {
			t.Fatal(err)
		}
		dg, err := SealInit(initKeys, rep, uint32(1000+i), inner, length)
		if err != nil {
			t.Fatal(err)
		}
		initBytes = append(initBytes, dg...)
	}

	dataBytes := make([]byte, 0, packets*length)
	payload := make([]byte, length-DataOverhead)
	for i := 0; i < packets; i++ {
		if _, err := rnd.Read(payload); err != nil {
			t.Fatal(err)
		}
		dg, err := SealData(keys.KI2R, keys.HPI2R, uint64(i), payload, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(dg) != length {
			t.Fatalf("data packet is %d bytes, want %d", len(dg), length)
		}
		dataBytes = append(dataBytes, dg...)
	}

	for name, stream := range map[string][]byte{"init": initBytes, "data": dataBytes} {
		if len(stream) < 1_000_000 {
			t.Fatalf("%s stream is %d bytes, want at least 10^6", name, len(stream))
		}
		stat := chiSquareBytes(stream)
		if stat > chiSquareP01 {
			t.Errorf("%s: chi-square %.1f exceeds %.3f (p < 0.01)", name, stat, chiSquareP01)
		}
		t.Logf("%s: %d bytes, chi-square %.1f (limit %.3f)", name, len(stream), stat, chiSquareP01)
	}
}

func FuzzOpenInit(f *testing.F) {
	v := loadVectors(f)
	f.Add(hx(f, v.Init.Datagram))
	f.Add(make([]byte, MinInitLen))
	f.Fuzz(func(t *testing.T, datagram []byte) {
		var keys Keys
		_, _ = OpenInit(keys, datagram)
	})
}

func FuzzOpenData(f *testing.F) {
	v := loadVectors(f)
	for _, d := range v.Data {
		f.Add(hx(f, d.Datagram))
	}
	f.Add(make([]byte, DataOverhead))
	f.Fuzz(func(t *testing.T, datagram []byte) {
		var key, hp [32]byte
		_, _, _ = OpenData(key, hp, datagram, 0)
		_, _, _ = OpenData(key, hp, datagram, 1<<40)
	})
}

func TestTestdataCopyMatchesSpec(t *testing.T) {
	spec := filepath.Join("..", "..", "..", "..", "..", "..", "docs", "protocol", "v2", "vectors", "noise3.json")
	if _, err := os.Stat(spec); err != nil {
		t.Skip("spec vectors not present (OSS copy)")
	}
	a, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(a, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))) {
		t.Error("testdata copy differs from docs/protocol/v2/vectors")
	}
}

func TestDecodeNumberExamples(t *testing.T) {
	// RFC 9000 Appendix A.3 worked example: largest = 0xa82f30ea, truncated = 0x9b32.
	if got := DecodeNumber(0xa82f30ea, 0x9b32, 16); got != 0xa82f9b32 {
		t.Fatalf("DecodeNumber = %#x, want %#x", got, 0xa82f9b32)
	}
}
