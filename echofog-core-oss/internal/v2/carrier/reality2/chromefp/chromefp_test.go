package chromefp

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
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

// storedSamples reads every stored sample of a transport from testdata.
func storedSamples(t *testing.T, transport string) []storedSample {
	t.Helper()
	files, _ := filepath.Glob("testdata/chrome-*-" + transport + ".json")
	out := make([]storedSample, 0, len(files))
	for _, f := range files {
		v := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "chrome-"), "-"+transport+".json")
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var s Sample
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out = append(out, storedSample{Version: v, Sample: &s})
	}
	return out
}

// newestSample loads the newest stored sample of a transport.
func newestSample(t *testing.T, transport string) *Sample {
	t.Helper()
	stored := storedSamples(t, transport)
	if len(stored) == 0 {
		t.Fatalf("no stored Chrome %s sample in testdata; run the live test with CHROMEFP_UPDATE=1", transport)
	}
	best := &stored[0]
	for i := range stored {
		if lessVer(versionKey(best.Version), versionKey(stored[i].Version)) {
			best = &stored[i]
		}
	}
	return best.Sample
}

func lessVer(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// storedSample is one sample read from testdata with the Chrome version it was taken from.
type storedSample struct {
	Version string
	Sample  *Sample
}

// selectSample picks the stored sample to compare a live Chrome of version live with. Only the
// same version is evidence about the installed browser: within one major version the newest
// stored sample is the closest one, and a sample of another major is the newest of all. exact
// reports the first case. A missing sample is a fault for the caller, not a licence to skip.
func selectSample(stored []storedSample, live string) (s *Sample, version string, exact bool) {
	if len(stored) == 0 {
		return nil, "", false
	}
	liveKey := versionKey(live)
	var sameMajor *storedSample
	newest := &stored[0]
	for i := range stored {
		s := &stored[i]
		k := versionKey(s.Version)
		if slices.Equal(k, liveKey) {
			return s.Sample, s.Version, true
		}
		if len(k) > 0 && len(liveKey) > 0 && k[0] == liveKey[0] {
			if sameMajor == nil || lessVer(versionKey(sameMajor.Version), k) {
				sameMajor = s
			}
		}
		if lessVer(versionKey(newest.Version), k) {
			newest = s
		}
	}
	if sameMajor != nil {
		return sameMajor.Sample, sameMajor.Version, false
	}
	return newest.Sample, newest.Version, false
}

// liveCheck is what one live capture proves: our profile against the installed Chrome, which is
// always checked, the drift line when the stored sample is of another version, and the installed
// Chrome against the stored sample of the same version.
type liveCheck struct {
	Divergences []string
	Drift       string
	Stored      []string
}

// checkLive compares our profile with the installed Chrome and, only when the stored sample is
// of the same version, the installed Chrome with that sample. A different version says nothing
// about the installed browser's hello: the drift is reported and only the stored comparison is
// skipped.
func checkLive(stored []storedSample, liveVersion string, live, ours *Sample) liveCheck {
	res := liveCheck{Divergences: Compare(live, ours)}
	s, version, exact := selectSample(stored, liveVersion)
	switch {
	case s == nil:
		res.Drift = fmt.Sprintf("no stored Chrome sample for %s in testdata: our profile was compared with the live Chrome only; run the live test with CHROMEFP_UPDATE=1", liveVersion)
	case !exact:
		res.Drift = fmt.Sprintf("stored sample is Chrome %s, installed %s: skipping only the stored-sample comparison, our profile is still checked against the live Chrome; refresh with CHROMEFP_UPDATE=1", version, liveVersion)
	default:
		res.Stored = Compare(s, live)
	}
	return res
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
// version into testdata. A stored sample of another version is a drift the runner does not control:
// it is logged and only the stored comparison is skipped, so our profile is still checked against
// the Chrome that is installed.
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
			diffs := checkLive(storedSamples(t, tc.transport), ver, live[0], parseOurs(t, tc.ours(t)))
			if k.Chrome != "" && k.Chrome != ver {
				t.Logf("known-divergences.json is for Chrome %s, this is %s", k.Chrome, ver)
			}
			if diffs.Drift != "" {
				t.Logf("%s", diffs.Drift)
			}
			if accept {
				// Writes the current divergences as the list awaiting review; the diff of this
				// file in git is the review.
				if tc.transport == "tcp" {
					accepted.TCP = diffs.Divergences
				} else {
					accepted.QUIC = diffs.Divergences
				}
				b, _ := json.MarshalIndent(accepted, "", " ")
				_ = os.WriteFile(knownFile, append(b, 10), 0o644)
				return
			}
			checkDivergences(t, diffs.Divergences, tc.listed)
			if d := diffs.Stored; len(d) != 0 {
				t.Errorf("the stored sample no longer matches the live Chrome %s: %v", ver, d)
			}
		})
	}
}

// TestSelectSample pins the choice of the stored sample: the exact version, the closest one of
// the same major when only the build differs, the newest one when the major is gone, and nothing
// when testdata is empty.
func TestSelectSample(t *testing.T) {
	s := func(ja4 string) *Sample { return &Sample{Source: "chrome", Transport: "tcp", JA4: ja4} }
	cases := []struct {
		name   string
		stored []storedSample
		live   string
		want   string
		exact  bool
	}{
		{
			name: "exact version",
			stored: []storedSample{
				{Version: "156.0.8078.81", Sample: s("a")},
				{Version: "157.0.1.1", Sample: s("b")},
			},
			live: "156.0.8078.81", want: "156.0.8078.81", exact: true,
		},
		{
			name:   "another build of the same major",
			stored: []storedSample{{Version: "156.0.8078.81", Sample: s("a")}},
			live:   "156.0.8078.79", want: "156.0.8078.81",
		},
		{
			name: "same major beats a newer major",
			stored: []storedSample{
				{Version: "155.0.9.9", Sample: s("a")},
				{Version: "156.0.8078.81", Sample: s("b")},
				{Version: "157.0.1.1", Sample: s("c")},
			},
			live: "156.0.8078.79", want: "156.0.8078.81",
		},
		{
			name: "no sample of this major",
			stored: []storedSample{
				{Version: "155.0.9.9", Sample: s("a")},
				{Version: "157.0.1.1", Sample: s("b")},
			},
			live: "156.0.8078.79", want: "157.0.1.1",
		},
		{
			name: "no stored sample at all",
			live: "156.0.8078.79",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, version, exact := selectSample(tc.stored, tc.live)
			if version != tc.want || exact != tc.exact {
				t.Fatalf("selectSample = %q exact=%v, want %q exact=%v", version, exact, tc.want, tc.exact)
			}
			if (got == nil) != (tc.want == "") {
				t.Fatalf("selectSample returned sample %v for version %q", got, tc.want)
			}
		})
	}
}

// TestCheckLive pins what a live capture proves: the version drift never excuses our profile from
// the comparison with the installed Chrome, and a sample of another version is not compared as if
// it described that Chrome.
func TestCheckLive(t *testing.T) {
	storedLive := &Sample{Source: "chrome", Transport: "tcp", JA4: "stored"}
	installed := &Sample{Source: "chrome", Transport: "tcp", JA4: "installed"}
	ours := &Sample{Source: "ours", Transport: "tcp", JA4: "ours"}

	t.Run("same version compares both layers", func(t *testing.T) {
		res := checkLive([]storedSample{{Version: "156.0.8078.81", Sample: storedLive}}, "156.0.8078.81", installed, ours)
		if res.Drift != "" {
			t.Fatalf("drift reported for the same version: %s", res.Drift)
		}
		if len(res.Divergences) != 1 {
			t.Fatalf("our profile was not compared with the live Chrome: %v", res.Divergences)
		}
		if len(res.Stored) != 1 {
			t.Fatalf("the live Chrome was not compared with the stored sample: %v", res.Stored)
		}
	})

	t.Run("another build reports drift and keeps the live comparison", func(t *testing.T) {
		res := checkLive([]storedSample{{Version: "156.0.8078.81", Sample: storedLive}}, "156.0.8078.79", installed, ours)
		if res.Drift == "" {
			t.Fatal("a stored sample of another version was not reported as drift")
		}
		if !strings.Contains(res.Drift, "156.0.8078.81") || !strings.Contains(res.Drift, "156.0.8078.79") {
			t.Fatalf("the drift line names neither version: %s", res.Drift)
		}
		if len(res.Divergences) != 1 {
			t.Fatalf("a version drift skipped our profile against the live Chrome: %v", res.Divergences)
		}
		if len(res.Stored) != 0 {
			t.Fatalf("a sample of another version was compared as the live browser: %v", res.Stored)
		}
	})

	t.Run("no stored sample", func(t *testing.T) {
		res := checkLive(nil, "156.0.8078.79", installed, ours)
		if res.Drift == "" {
			t.Fatal("a missing stored sample was not reported")
		}
		if len(res.Divergences) != 1 {
			t.Fatalf("a missing stored sample skipped our profile against the live Chrome: %v", res.Divergences)
		}
		if len(res.Stored) != 0 {
			t.Fatalf("a missing stored sample produced a stored comparison: %v", res.Stored)
		}
	})
}
