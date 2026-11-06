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
		"https://join.example.org/join/#v=2&card=AAAA", "", "echofog://", "https://", "(\"echofog://join?x\")",
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
