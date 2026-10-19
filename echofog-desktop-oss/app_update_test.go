package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.1", "0.1.0", true},
		{"v0.2.0", "0.1.9", true},
		{"1.2.10", "1.2.9", true},
		{"0.1.0", "0.1.0", false},
		{"0.1.0", "0.1.1", false},
		{"1.3.0", "1.3.0-rc1", true},
		{"1.3.0-rc1", "1.3.0", false},
		{"1.3", "1.2.9", true},
	}
	for _, c := range cases {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestFetchLatestRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "need a user agent", http.StatusForbidden)
			return
		}
		w.Write([]byte(`{"tag_name":"v0.1.1","html_url":"https://github.com/AdmiralCore/EchoFog/releases/tag/v0.1.1","draft":false}`))
	}))
	defer srv.Close()
	tag, page, err := fetchLatestRelease(context.Background(), http.DefaultClient, srv.URL)
	if err != nil || tag != "v0.1.1" || page != "https://github.com/AdmiralCore/EchoFog/releases/tag/v0.1.1" {
		t.Fatalf("got %q %q %v", tag, page, err)
	}

	// a page link that is not GitHub's is never handed to the browser
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"https://example.com/malware.exe"}`))
	}))
	defer evil.Close()
	_, page, err = fetchLatestRelease(context.Background(), http.DefaultClient, evil.URL)
	if err != nil || page != "https://github.com/AdmiralCore/EchoFog/releases/latest" {
		t.Fatalf("foreign link not replaced: %q %v", page, err)
	}

	none := httptest.NewServer(http.NotFoundHandler())
	defer none.Close()
	if _, _, err := fetchLatestRelease(context.Background(), http.DefaultClient, none.URL); err == nil {
		t.Fatal("no release must be an error, not a version")
	}
}
