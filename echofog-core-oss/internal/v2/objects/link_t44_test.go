package objects

import (
	"errors"
	"strings"
	"testing"
)

// TestParseLinkSlashForm: v1 links were pasted as echofog://join/?...; the v2 decoder takes the
// slash in both forms (T4.4). The encoder never writes it.
func TestParseLinkSlashForm(t *testing.T) {
	t.Parallel()
	lv := loadVector(t, "link.json")
	contact := str(t, lv, "contact", "link")
	want, err := ParseLink(contact)
	if err != nil {
		t.Fatal(err)
	}
	slash := strings.Replace(contact, "echofog://join?", "echofog://join/?", 1)
	web := WebLink("join.example.org", contact)
	webSlash := strings.Replace(web, "/join/#join?", "/join/#join/?", 1)
	for _, in := range []string{slash, webSlash} {
		got, err := ParseLink(in)
		if err != nil {
			t.Fatalf("ParseLink(%.50q): %v", in, err)
		}
		if string(got.Card) != string(want.Card) {
			t.Fatalf("ParseLink(%.50q): other card", in)
		}
	}
	for _, in := range []string{"echofog://join//?v=2", "echofog://join/x?v=2&card=AAAA"} {
		if _, err := ParseLink(in); !errors.Is(err, ErrBadLink) {
			t.Errorf("ParseLink(%q): %v", in, err)
		}
	}
}

// TestParseLinkBoundsTheWholeLink: the card parameter is bounded, the rest of the link was not.
func TestParseLinkBoundsTheWholeLink(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"parameters": "echofog://join?v=2" + strings.Repeat("&x=1", 200000),
		"unknown":    "echofog://join?v=2&x=" + strings.Repeat("A", 1<<20) + "&card=AAAA",
		"web":        "https://h/join/#join?v=2&x=" + strings.Repeat("A", 1<<20),
	} {
		if _, err := ParseLink(in); !errors.Is(err, ErrBadLink) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
