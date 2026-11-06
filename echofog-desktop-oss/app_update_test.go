package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/echofog/echofog-core/pkg/api/node"
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

func TestUpdateInfoFromTheManifest(t *testing.T) {
	if got := updateInfoFromNotice("0.3.0", nil); got.Newer || got.Current != "0.3.0" || got.Latest != "" {
		t.Fatalf("no notice: %+v", got)
	}
	n := &api.UpdateNotice{Version: "0.3.1", NotesURL: "https://example.org/notes", Important: true, Components: []string{"proto"}}
	got := updateInfoFromNotice("0.3.0", n)
	want := UpdateInfo{Current: "0.3.0", Latest: "0.3.1", URL: "https://example.org/notes", Newer: true, Important: true, Components: []string{"proto"}}
	if got.Current != want.Current || got.Latest != want.Latest || got.URL != want.URL || !got.Newer || !got.Important || len(got.Components) != 1 {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// the manifest says a component is behind even when the app version is the same
	if got := updateInfoFromNotice("0.3.1", &api.UpdateNotice{Version: "0.3.1"}); !got.Newer {
		t.Fatalf("same version, behind component: %+v", got)
	}
	// a page that is not https is never handed to the browser: the releases page stands in
	for _, bad := range []string{"", "http://example.org/n", "javascript:alert(1)", "file:///c:/x"} {
		got := updateInfoFromNotice("0.3.0", &api.UpdateNotice{Version: "0.3.1", NotesURL: bad})
		if got.URL != "https://github.com/AdmiralCore/EchoFog/releases/latest" {
			t.Errorf("%q: url %q", bad, got.URL)
		}
	}
	// the version text is shown, so it is plain: no markup, no control characters
	got = updateInfoFromNotice("0.3.0", &api.UpdateNotice{Version: "0.3.1<script>\n"})
	if strings.ContainsAny(got.Latest, "<>\n") {
		t.Fatalf("latest %q", got.Latest)
	}
}

func TestNoticePayload(t *testing.T) {
	p := noticePayload("clock-skew", "Часы", nil)
	if p["kind"] != "clock-skew" || p["text"] != "Часы" || p["update"] != nil {
		t.Fatalf("payload: %+v", p)
	}
	p = noticePayload("update", "x", &api.UpdateNotice{Version: "0.3.1", NotesURL: "https://example.org/n", Important: true})
	u, ok := p["update"].(UpdateInfo)
	if !ok || u.Latest != "0.3.1" || !u.Manifest || !u.Important || u.URL != "https://example.org/n" {
		t.Fatalf("payload: %+v", p)
	}
}
