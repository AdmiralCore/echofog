package chromefp

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Stored Chrome samples live in testdata/chrome-<version>-<tcp|quic>.json, taken by the live test
// (CHROMEFP_UPDATE=1, see docs/protocol/v2/chrome-fingerprint.md). Divergences the architect has
// seen and not yet decided on live in testdata/known-divergences.json: a divergence outside the
// list fails, and so does a listed one that is gone, so the list cannot go stale unnoticed.

const knownFile = "testdata/known-divergences.json"

type known struct {
	Chrome string   `json:"chrome"`
	TCP    []string `json:"tcp"`
	QUIC   []string `json:"quic"`
}

func versionKey(v string) []int {
	var k []int
	for _, p := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(p)
		k = append(k, n)
	}
	return k
}

// newestSample loads the newest stored sample of a transport.
func newestSample(t *testing.T, transport string) *Sample {
	t.Helper()
	files, _ := filepath.Glob("testdata/chrome-*-" + transport + ".json")
	if len(files) == 0 {
		t.Fatalf("no stored Chrome %s sample in testdata; run the live test with CHROMEFP_UPDATE=1", transport)
	}
	best, bestKey := "", []int(nil)
	for _, f := range files {
		v := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "chrome-"), "-"+transport+".json")
		k := versionKey(v)
		if best == "" || lessVer(bestKey, k) {
			best, bestKey = f, k
		}
	}
	b, err := os.ReadFile(best)
	if err != nil {
		t.Fatal(err)
	}
	var s Sample
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: %v", best, err)
	}
	return &s
}

func lessVer(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func loadKnown(t *testing.T) known {
	t.Helper()
	var k known
	if b, err := os.ReadFile(knownFile); err == nil {
		if err := json.Unmarshal(b, &k); err != nil {
			t.Fatalf("%s: %v", knownFile, err)
		}
	}
	return k
}

// checkDivergences fails on a divergence that is not listed and on a listed one that is gone.
func checkDivergences(t *testing.T, got, listed []string) {
	t.Helper()
	have := map[string]bool{}
	for _, d := range got {
		have[d] = true
	}
	want := map[string]bool{}
	for _, d := range listed {
		want[d] = true
		if !have[d] {
			t.Errorf("listed divergence is gone (remove it from %s): %s", knownFile, d)
		}
	}
	for _, d := range got {
		if !want[d] {
			t.Errorf("NEW divergence from Chrome: %s", d)
		}
	}
}

func parseOurs(t *testing.T, hello []byte) *Sample {
	t.Helper()
	s, err := Parse(hello)
	if err != nil {
		t.Fatal(err)
	}
	s.Source = "ours"
	return s
}

// The CI check: our profile against the stored Chrome sample. No browser needed.
func TestProfileMatchesStoredChromeSample(t *testing.T) {
	k := loadKnown(t)
	for _, tc := range []struct {
		transport string
		ours      func(*testing.T) []byte
		listed    []string
	}{{"tcp", ourTCP, k.TCP}, {"quic", ourQUIC, k.QUIC}} {
		t.Run(tc.transport, func(t *testing.T) {
			want := newestSample(t, tc.transport)
			checkDivergences(t, Compare(want, parseOurs(t, tc.ours(t))), tc.listed)
		})
	}
}

// An outdated profile must be red: the TCP hello of Chrome 120 differs from the current sample.
func TestStaleProfileIsRed(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	uc := utls.UClient(cli, &utls.Config{ServerName: sampleHost, InsecureSkipVerify: true}, utls.HelloChrome_120)
	go func() { _ = uc.Handshake() }()
	_ = srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	h := readTLSClientHello(srv)
	if h == nil {
		t.Fatal("no hello from the Chrome 120 profile")
	}
	diffs := Compare(newestSample(t, "tcp"), parseOurs(t, h))
	if len(diffs) == 0 {
		t.Fatal("a Chrome 120 profile compared equal to the current Chrome sample")
	}
	// ... and it is not the allowed list that excuses it
	if fmt.Sprint(diffs) == fmt.Sprint(loadKnown(t).TCP) {
		t.Fatal("the stale profile shows only the known divergences")
	}
}

// The profile of our own samples is stable from one build to the next.
func TestOursIsDeterministicInShape(t *testing.T) {
	a, b := parseOurs(t, ourTCP(t)), parseOurs(t, ourTCP(t))
	if d := Compare(a, b); len(d) != 0 {
		t.Fatalf("two builds of our TCP hello differ: %v", d)
	}
	qa, qb := parseOurs(t, ourQUIC(t)), parseOurs(t, ourQUIC(t))
	if d := Compare(qa, qb); len(d) != 0 {
		t.Fatalf("two builds of our QUIC hello differ: %v", d)
	}
}

// The live check: the installed stable Chrome, headless, against a loopback server. Nothing leaves
// the machine. Skipped without Chrome. CHROMEFP_UPDATE=1 writes the samples of the installed
// version into testdata.
func TestLiveChrome(t *testing.T) {
	path, ver := findChrome()
	if path == "" {
		t.Skip("no stable Chrome on this machine (set CHROME_BIN); the stored samples still gate CI")
	}
	update := os.Getenv("CHROMEFP_UPDATE") == "1"
	accept := os.Getenv("CHROMEFP_ACCEPT") == "1"
	k := loadKnown(t)
	accepted := known{Chrome: ver}
	for _, tc := range []struct {
		transport string
		capture   func(*testing.T, string) []byte
		ours      func(*testing.T) []byte
		listed    []string
	}{{"tcp", captureChromeTCP, ourTCP, k.TCP}, {"quic", captureChromeQUIC, ourQUIC, k.QUIC}} {
		t.Run(tc.transport, func(t *testing.T) {
			var live []*Sample
			for i := 0; i < 3; i++ { // Chrome permutes extensions and draws ECH padding per connection
				s, err := Parse(tc.capture(t, path))
				if err != nil {
					t.Fatal(err)
				}
				s.Source, s.Chrome = "chrome", ver
				live = append(live, s)
			}
			for i := 1; i < len(live); i++ {
				if d := Compare(live[0], live[i]); len(d) != 0 {
					t.Fatalf("Chrome %s is not stable between connections: %v", ver, d)
				}
			}
			if update {
				b, _ := json.MarshalIndent(live[0], "", " ")
				name := fmt.Sprintf("testdata/chrome-%s-%s.json", ver, tc.transport)
				if err := os.WriteFile(name, append(b, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("wrote %s", name)
			}
			diffs := Compare(live[0], parseOurs(t, tc.ours(t)))
			if k.Chrome != "" && k.Chrome != ver {
				t.Logf("known-divergences.json is for Chrome %s, this is %s", k.Chrome, ver)
			}
			if accept {
				// Writes the current divergences as the list awaiting review; the diff of this
				// file in git is the review.
				if tc.transport == "tcp" {
					accepted.TCP = diffs
				} else {
					accepted.QUIC = diffs
				}
				b, _ := json.MarshalIndent(accepted, "", " ")
				_ = os.WriteFile(knownFile, append(b, 10), 0o644)
				return
			}
			checkDivergences(t, diffs, tc.listed)
			stored := newestSample(t, tc.transport)
			if stored.Chrome != ver {
				t.Errorf("stored sample is Chrome %s, installed %s: refresh with CHROMEFP_UPDATE=1", stored.Chrome, ver)
			} else if d := Compare(stored, live[0]); len(d) != 0 {
				t.Errorf("the stored sample no longer matches the live Chrome %s: %v", ver, d)
			}
		})
	}
}
