package app_test

import (
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/app"
)

// FuzzExtractLink: pasted text comes from anywhere. ExtractLink must not panic, must return a
// bounded token without white space that starts like a link, and must be a substring of the input.
func FuzzExtractLink(f *testing.F) {
	for _, s := range []string{
		"echofog://join?v=2&card=AAAA", "см. echofog://join/?v=2&card=AAAA&s=BB, ок",
		"https://join.example.org/join/#join?v=2&card=AAAA", "", "echofog://", "https://", "(\"echofog://join?x\")",
		strings.Repeat("echofog://", 50), "echofog://\x00\xff", "https://a/join/#" + strings.Repeat("A", 1<<16),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got, ok := app.ExtractLink(in)
		if !ok {
			if got != "" {
				t.Fatalf("not found but %q returned", got)
			}
			return
		}
		if !strings.Contains(in, got) || len(got) > 16<<10 || strings.ContainsAny(got, " \t\r\n") {
			t.Fatalf("bad token %q from %q", got, in)
		}
		if !strings.HasPrefix(got, "echofog://") && !strings.HasPrefix(got, "https://") {
			t.Fatalf("token %q does not look like a link", got)
		}
	})
}

// FuzzUnwebInvite: the web form of a group invitation comes from anywhere. It must not panic, must not
// grow the input beyond the prefix, and must take only data that sits in the fragment of the join page.
func FuzzUnwebInvite(f *testing.F) {
	for _, s := range []string{
		"https://join.example.org/join/#group/AAAA", "https://join.example.org/join/?group/AAAA", "https://u@h/join/#group/AA",
		"https://h/join/#group/", "https://h/join/#contact/AA", "echofog-group:2:AAAA", "", "https://", "https:///join/#group/x",
		"https://h/x/join/#group/y", "https://h\x5c@x/join/#group/y", strings.Repeat("https://", 100),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := app.UnwebInviteForTest(in)
		if got == in {
			return
		}
		if !strings.HasPrefix(got, "echofog-group:2:") || !strings.HasPrefix(in, "https://") || !strings.Contains(in, "/join/#group/") {
			t.Fatalf("%q became %q", in, got)
		}
		host := in[len("https://"):strings.Index(in, "/join/#group/")]
		if host == "" || strings.ContainsAny(host, "/?#@\x5c ") {
			t.Fatalf("a host %q was taken", host)
		}
		if len(got) > len(in) {
			t.Fatalf("the result is longer than the input")
		}
	})
}

// FuzzOpenSnapshot: a group file comes from anywhere. A file that is not sealed with the password never
// opens, and no input makes the opener panic.
func FuzzOpenSnapshot(f *testing.F) {
	for _, s := range []string{"", "FOGZIP1\n", "FOGZIP2\n", "FOGZIP9\n" + strings.Repeat("a", 100), "FOGZIP2\n" + strings.Repeat("\x00", 80),
		strings.Repeat("FOGZIP2\n", 20), "hello"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) > 4096 {
			return // keep the key derivation of a long garbage out of the loop
		}
		if d, err := app.OpenSnapshotForTest(in, "long enough pass"); err == nil {
			t.Fatalf("a made-up file opened: %+v", d)
		}
	})
}
