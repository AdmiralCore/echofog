package punch

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestRendezvousIDVector(t *testing.T) {
	raw, err := os.ReadFile("testdata/kdf.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Relay2 struct {
			Day    uint32 `json:"day"`
			Secret string `json:"personal_pass_secret"`
			RID    string `json:"rendezvous_id"`
		} `json:"relay2"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	secret, _ := hex.DecodeString(v.Relay2.Secret)
	want, _ := hex.DecodeString(v.Relay2.RID)
	got := RendezvousID(secret, v.Relay2.Day)
	if !bytes.Equal(got[:], want) {
		t.Fatalf("rid = %x, want %x", got, want)
	}
}

// The copy of the spec vector must equal the spec.
func TestKDFVectorCopyMatchesSpec(t *testing.T) {
	a, err := os.ReadFile("testdata/kdf.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../../../docs/protocol/v2/vectors/kdf.json")
	if err != nil {
		t.Fatal(err)
	}
	norm := func(x []byte) []byte { return bytes.ReplaceAll(x, []byte("\r\n"), []byte("\n")) }
	if !bytes.Equal(norm(a), norm(b)) {
		t.Fatal("testdata/kdf.json differs from docs/protocol/v2/vectors/kdf.json")
	}
}

func TestDay(t *testing.T) {
	cases := []struct {
		at   time.Time
		want uint32
	}{
		{time.Unix(0, 0), 0},
		{time.Unix(86399, 0), 0},
		{time.Unix(86400, 0), 1},
		{time.Unix(20760*86400+5, 0), 20760},
		{time.Unix(20760*86400, 0).In(time.FixedZone("x", 5*3600)), 20760},
	}
	for _, c := range cases {
		if got := Day(c.at); got != c.want {
			t.Errorf("Day(%v) = %d, want %d", c.at, got, c.want)
		}
	}
}

func TestRendezvousIDDistinct(t *testing.T) {
	a, b := []byte("0123456789abcdef"), []byte("0123456789abcdeg")
	if RendezvousID(a, 5) == RendezvousID(b, 5) || RendezvousID(a, 5) == RendezvousID(a, 6) {
		t.Fatal("collision")
	}
	if RendezvousID(a, 5) != RendezvousID(a, 5) {
		t.Fatal("not deterministic")
	}
}

// IDs registers the current and the next day for every secret (services.md §6.3), de-duplicated
// and capped at maxIDs; a secret that does not fit whole (both days) is dropped.
func TestIDsTodayAndTomorrow(t *testing.T) {
	now := time.Unix(20760*86400+100, 0)
	s1, s2 := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	ids := IDs([][]byte{s1, s2, s1}, now, 256)
	if len(ids.Today) != 2 || len(ids.Tomorrow) != 2 {
		t.Fatalf("today %d tomorrow %d, want 2 and 2", len(ids.Today), len(ids.Tomorrow))
	}
	if ids.Day != 20760 {
		t.Fatalf("day %d", ids.Day)
	}
	if ids.Today[0] != RendezvousID(s1, 20760) || ids.Tomorrow[1] != RendezvousID(s2, 20761) {
		t.Fatal("wrong ids")
	}
	capped := IDs([][]byte{s1, s2}, now, 3)
	if len(capped.Today)+len(capped.Tomorrow) > 3 || len(capped.Today) != len(capped.Tomorrow) || len(capped.Today) != 1 {
		t.Fatalf("cap broken: %d + %d", len(capped.Today), len(capped.Tomorrow))
	}
}
