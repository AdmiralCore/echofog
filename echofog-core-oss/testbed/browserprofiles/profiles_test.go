package browserprofiles

import (
	"path/filepath"
	"testing"
)

func TestCaptureChromePresetLooksLikeChrome(t *testing.T) {
	p, err := captureTLSChrome()
	if err != nil {
		t.Fatalf("capture the Chrome preset: %v", err)
	}
	if p.Source != SourceUTLSChrome {
		t.Errorf("source = %q, want %q", p.Source, SourceUTLSChrome)
	}
	if p.TLS == nil || p.TLS.JA4 == "" || p.TLS.JA3 == "" {
		t.Fatalf("no fingerprints: %+v", p.TLS)
	}
	if !p.TLS.Grease {
		t.Error("the Chrome preset has no GREASE")
	}
	if p.TLS.ALPN == "" {
		t.Error("no ALPN recorded")
	}
}

func TestDatabaseRoundTripAndBrowserSet(t *testing.T) {
	db := &Database{
		Version: "2026.11.1",
		Entries: []Profile{
			{ID: "chrome-utls", Source: SourceUTLSChrome, Country: "lab", Network: "lab",
				TLS: &TLSProfile{JA3: "a", JA4: "t13d1516h2_browser", Grease: true}},
			{ID: "curl-lab", Source: SourceCurl, Country: "lab", Network: "lab",
				TLS: &TLSProfile{JA3: "b", JA4: "t13d1516h2_tool", Grease: false}},
		},
	}
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := db.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != db.Version || len(got.Entries) != 2 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	set := got.BrowserJA4()
	if !set["t13d1516h2_browser"] {
		t.Error("the Chrome profile is not in the browser set")
	}
	if set["t13d1516h2_tool"] {
		t.Error("curl was counted as a browser")
	}
}
