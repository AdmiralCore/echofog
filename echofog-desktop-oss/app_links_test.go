package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode"
)

func TestLinkFromArgs(t *testing.T) {
	v2 := "echofog://join?v=2&card=AAAA&s=BBBB"
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"plain", []string{v2}, v2},
		{"after other arguments", []string{"--minimized", v2, "x"}, v2},
		{"spaces around", []string{"  " + v2 + "\t"}, v2},
		{"upper case scheme", []string{"ECHOFOG://join?v=2&card=A"}, "ECHOFOG://join?v=2&card=A"},
		{"slash before the question mark", []string{"echofog://join/?v=2&card=A"}, "echofog://join/?v=2&card=A"},
		{"scheme without slashes", []string{"echofog:join?v=2&card=A"}, "echofog://join?v=2&card=A"},
		{"scheme with one slash", []string{"echofog:/join?v=2&card=A"}, "echofog://join?v=2&card=A"},
		{"relay link", []string{"echofog://relay/1.2.3.4:5/abc"}, "echofog://relay/1.2.3.4:5/abc"},
		{"no link", []string{"--flag", "C:/x/y.exe"}, ""},
		{"empty", nil, ""},
		{"only the scheme", []string{"echofog://"}, ""},
		{"another scheme", []string{"http://echofog://join"}, ""},
		{"control characters", []string{"echofog://join?v=2\r\nX-Evil: 1"}, ""},
		{"nul", []string{"echofog://join?v=2\x00"}, ""},
		{"too long", []string{"echofog://join?v=2&card=" + strings.Repeat("A", maxLinkBytes)}, ""},
		{"just fits", []string{"echofog://join?card=" + strings.Repeat("A", maxLinkBytes-len("echofog://join?card="))}, "echofog://join?card=" + strings.Repeat("A", maxLinkBytes-len("echofog://join?card="))},
	}
	for _, c := range cases {
		if got := linkFromArgs(c.args); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, short(got), short(c.want))
		}
	}
}

func short(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func FuzzLinkFromArgs(f *testing.F) {
	for _, s := range []string{"echofog://join?v=2&card=A", "echofog:join/?x", "ECHOFOG:/y", "echofog://", "", "echofog://\x00", "x echofog://a"} {
		f.Add(s, "--flag")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		got := linkFromArgs([]string{a, b})
		if got == "" {
			return
		}
		if len(got) > maxLinkBytes || !strings.HasPrefix(strings.ToLower(got), linkScheme) || len(got) == len(linkScheme) {
			t.Fatalf("bad link %q from %q %q", short(got), short(a), short(b))
		}
		for _, r := range got {
			if unicode.IsControl(r) {
				t.Fatalf("control character in %q", short(got))
			}
		}
		if again := linkFromArgs([]string{got}); again != got {
			t.Fatalf("not stable: %q then %q", short(got), short(again))
		}
	})
}

func TestLinkHandoffHandler(t *testing.T) {
	var opened []string
	h := linkHandoffHandler("tok", func(l string) { opened = append(opened, l) })
	post := func(token, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/link", strings.NewReader(body))
		if token != "" {
			req.Header.Set("X-EchoFog-Token", token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	v2 := "echofog://join?v=2&card=" + strings.Repeat("A", 12<<10) // a card link of a size the old 8 KiB limit cut
	if c := post("tok", v2); c != http.StatusNoContent || len(opened) != 1 || opened[0] != v2 {
		t.Fatalf("a 12 KiB link: %d, opened %d", c, len(opened))
	}
	if c := post("tok", "echofog:join/?v=2&card=A"); c != http.StatusNoContent || opened[1] != "echofog://join/?v=2&card=A" {
		t.Fatalf("scheme without slashes: %d %v", c, opened)
	}
	n := len(opened)
	if c := post("tok", "echofog://join?card="+strings.Repeat("A", maxLinkBytes)); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("a link over the limit: %d", c)
	}
	if c := post("wrong", "echofog://join?v=2"); c != http.StatusNotFound {
		t.Fatalf("wrong token: %d", c)
	}
	if c := post("", "echofog://join?v=2"); c != http.StatusNotFound {
		t.Fatalf("no token: %d", c)
	}
	if c := post("tok", "hello"); c != http.StatusBadRequest {
		t.Fatalf("not a link: %d", c)
	}
	req := httptest.NewRequest(http.MethodGet, "/link", nil)
	req.Header.Set("X-EchoFog-Token", "tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET: %d", rec.Code)
	}
	if len(opened) != n {
		t.Fatalf("a refused request opened a link: %v", opened[n:])
	}
}
