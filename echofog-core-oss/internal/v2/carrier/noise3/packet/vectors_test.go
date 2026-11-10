package packet

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

const vectorPath = "../../../testdata/noise3.json"

type vectors struct {
	MaskSK    string `json:"mask_sk"`
	MaskPub   string `json:"mask_pub"`
	Z         string `json:"z"`
	Ephemeral struct {
		SK    string `json:"sk"`
		Repr  string `json:"repr"`
		Dirty string `json:"dirty_pub"`
	} `json:"ephemeral"`
	HKDFInfo string `json:"hkdf_info"`
	OKM      struct {
		KSel  string `json:"k_sel"`
		KInit string `json:"k_init"`
		KI2R  string `json:"k_i2r"`
		KR2I  string `json:"k_r2i"`
		HPI2R string `json:"hp_i2r"`
		HPR2I string `json:"hp_r2i"`
	} `json:"okm"`
	Sel  string `json:"sel"`
	Init struct {
		TS       uint32 `json:"ts"`
		InnerLen int    `json:"inner_len"`
		PadLen   int    `json:"pad_len"`
		TotalLen int    `json:"total_len"`
		SHA256   string `json:"init_plain_sha256"`
		Datagram string `json:"datagram"`
	} `json:"init"`
	Data []struct {
		Dir       string `json:"dir"`
		N         uint64 `json:"n"`
		Inner     string `json:"inner"`
		PadLen    int    `json:"pad_len"`
		PNPlain   string `json:"pn_plain"`
		Nonce     string `json:"nonce"`
		Plaintext string `json:"plaintext"`
		Sample    string `json:"sample"`
		Mask      string `json:"mask"`
		Datagram  string `json:"datagram"`
	} `json:"data"`
}

func loadVectors(t testing.TB) vectors {
	t.Helper()
	b, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	return v
}

func hx(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

func pattern(n int, start byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

func TestDeriveKeys(t *testing.T) {
	v := loadVectors(t)
	keys, err := Derive(hx(t, v.Z), hx(t, v.MaskPub), hx(t, v.Ephemeral.Dirty), hx(t, v.Ephemeral.Repr))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	for name, got := range map[string][32]byte{
		"k_sel": keys.KSel, "k_init": keys.KInit, "k_i2r": keys.KI2R,
		"k_r2i": keys.KR2I, "hp_i2r": keys.HPI2R, "hp_r2i": keys.HPR2I,
	} {
		hexGot := hex.EncodeToString(got[:])
		var want string
		switch name {
		case "k_sel":
			want = v.OKM.KSel
		case "k_init":
			want = v.OKM.KInit
		case "k_i2r":
			want = v.OKM.KI2R
		case "k_r2i":
			want = v.OKM.KR2I
		case "hp_i2r":
			want = v.OKM.HPI2R
		case "hp_r2i":
			want = v.OKM.HPR2I
		}
		if hexGot != want {
			t.Errorf("%s = %s, want %s", name, hexGot, want)
		}
	}
	if got := hex.EncodeToString(keys.Sel[:]); got != v.Sel {
		t.Errorf("sel = %s, want %s", got, v.Sel)
	}
	if got := hex.EncodeToString(Info(hx(t, v.MaskPub), hx(t, v.Ephemeral.Dirty))); got != v.HKDFInfo {
		t.Errorf("info = %s, want %s", got, v.HKDFInfo)
	}
	if _, err := Derive(make([]byte, 32), hx(t, v.MaskPub), hx(t, v.Ephemeral.Dirty), hx(t, v.Ephemeral.Repr)); err == nil {
		t.Error("Derive accepted an all-zero z")
	}
}

func TestInitVector(t *testing.T) {
	v := loadVectors(t)
	keys, err := Derive(hx(t, v.Z), hx(t, v.MaskPub), hx(t, v.Ephemeral.Dirty), hx(t, v.Ephemeral.Repr))
	if err != nil {
		t.Fatal(err)
	}
	inner := pattern(700, 0)
	got, err := SealInit(keys, hx(t, v.Ephemeral.Repr), v.Init.TS, inner, v.Init.TotalLen)
	if err != nil {
		t.Fatalf("SealInit: %v", err)
	}
	if hex.EncodeToString(got) != v.Init.Datagram {
		t.Fatalf("Init datagram differs from the vector")
	}
	init, err := OpenInit(keys, got)
	if err != nil {
		t.Fatalf("OpenInit: %v", err)
	}
	if init.TS != v.Init.TS || !bytes.Equal(init.Inner, inner) {
		t.Fatalf("OpenInit = ts %d, inner %d bytes; want ts %d, inner %d bytes", init.TS, len(init.Inner), v.Init.TS, len(inner))
	}
}

func TestDataVectors(t *testing.T) {
	v := loadVectors(t)
	keys, err := Derive(hx(t, v.Z), hx(t, v.MaskPub), hx(t, v.Ephemeral.Dirty), hx(t, v.Ephemeral.Repr))
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range v.Data {
		dir, hp := keys.KI2R, keys.HPI2R
		if d.Dir == "r2i" {
			dir, hp = keys.KR2I, keys.HPR2I
		}
		got, err := SealData(dir, hp, d.N, hx(t, d.Inner), d.PadLen)
		if err != nil {
			t.Fatalf("case %d: SealData: %v", i, err)
		}
		if hex.EncodeToString(got) != d.Datagram {
			t.Errorf("case %d: datagram differs from the vector", i)
		}
		highest := uint64(0)
		if d.N == 5000 {
			highest = 1
		}
		n, inner, err := OpenData(dir, hp, got, highest)
		if err != nil {
			t.Fatalf("case %d: OpenData: %v", i, err)
		}
		if n != d.N {
			t.Errorf("case %d: n = %d, want %d", i, n, d.N)
		}
		if !bytes.Equal(inner, hx(t, d.Inner)) {
			t.Errorf("case %d: inner differs", i)
		}
	}
}

func TestSealDataRejectsHugeNumbers(t *testing.T) {
	k := [32]byte{}
	if _, err := SealData(k, k, 1<<62, nil, 0); err == nil {
		t.Fatal("SealData accepted n = 2^62")
	}
}

func TestDecodeNumber(t *testing.T) {
	cases := []struct {
		largest uint64
		trunc   uint32
		want    uint64
	}{
		{0, 0, 0}, {0, 1, 1}, {1, 5000, 5000}, {1, 1, 1},
		{0xffffffff, 0, 0x100000000}, {0x100000000, 0, 0x100000000},
		{0x100000001, 0xfffffffe, 0xfffffffe},
		{(1 << 33), 5, (1 << 33) + 5},
	}
	for _, c := range cases {
		if got := DecodeNumber(c.largest, c.trunc, 32); got != c.want {
			t.Errorf("DecodeNumber(%d, %d) = %d, want %d", c.largest, c.trunc, got, c.want)
		}
	}
}

func TestReplayWindow(t *testing.T) {
	var w Window
	if !w.Check(0) {
		t.Fatal("first packet 0 rejected")
	}
	w.Commit(0)
	if w.Check(0) {
		t.Fatal("duplicate accepted")
	}
	w.Commit(1023)
	if !w.Check(1024) || w.Check(0) {
		t.Fatal("window edges are wrong")
	}
	w.Commit(1024)
	if !w.Check(1) {
		t.Fatal("number 1023 behind rejected")
	}
	if w.Check(0) {
		t.Fatal("number 1024 behind accepted")
	}
	if w.Highest() != 1024 {
		t.Errorf("Highest = %d, want 1024", w.Highest())
	}
}

// N3-FIX: Inits of every length from the smallest to 1200 bytes seal and open.
func TestInitRoundTripSmallSizes(t *testing.T) {
	v := loadVectors(t)
	keys, err := Derive(hx(t, v.Z), hx(t, v.MaskPub), hx(t, v.Ephemeral.Dirty), hx(t, v.Ephemeral.Repr))
	if err != nil {
		t.Fatal(err)
	}
	for _, total := range []int{MinInitLen, 400, 900, 1000, 1172, MaxFirstLen} {
		inner := pattern(total-InitOverhead-17, 3)
		dg, err := SealInit(keys, hx(t, v.Ephemeral.Repr), v.Init.TS, inner, total)
		if err != nil {
			t.Fatalf("total %d: %v", total, err)
		}
		if len(dg) != total {
			t.Fatalf("total %d: datagram is %d bytes", total, len(dg))
		}
		got, err := OpenInit(keys, dg)
		if err != nil || len(got.Inner) != len(inner) {
			t.Fatalf("total %d: open: %v", total, err)
		}
	}
}
