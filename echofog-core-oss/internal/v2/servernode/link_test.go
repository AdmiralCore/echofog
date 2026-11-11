package servernode_test

import (
	"strings"
	"testing"
)

// TestCLILinkCheck covers the offline support tool: it names the reason a link does not open,
// without a profile and without the network, and never prints the secret.
func TestCLILinkCheck(t *testing.T) {
	if code, _, _ := runCLI(nil, "link"); code != 2 {
		t.Fatalf("bare link: %d, want a usage error", code)
	}
	if code, _, _ := runCLI(nil, "link", "check"); code != 2 {
		t.Fatalf("link check without a link: %d, want a usage error", code)
	}

	code, out, _ := runCLI(nil, "link", "check", "hello")
	if code == 0 || !strings.Contains(out, "not a link") {
		t.Fatalf("plain text: %d %q", code, out)
	}
	code, out, _ = runCLI(nil, "link", "check", "echofog://join?v=3&card=AAAA")
	if code == 0 || !strings.Contains(out, "other version") {
		t.Fatalf("another version: %d %q", code, out)
	}

	dir, _ := profile(t, true)
	startServer(t, dir)
	code, out, errs := runCLI(nil, "invite", "create", "--data", dir, "--access", "30d")
	if code != 0 {
		t.Fatalf("invite create: %d %q", code, errs)
	}
	link := ""
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "link:         "); ok {
			link = strings.TrimSpace(v)
		}
	}
	if link == "" {
		t.Fatalf("invite create printed no link:\n%s", out)
	}
	code, out, errs = runCLI(nil, "link", "check", link)
	if code != 0 {
		t.Fatalf("a fresh invitation link: %d %q %q", code, out, errs)
	}
	if !strings.Contains(out, "ok: invitation link") || !strings.Contains(out, "valid until") {
		t.Fatalf("link check verdict %q", out)
	}
	if strings.Contains(out, link) {
		t.Fatal("link check printed the link back")
	}

	// The same verdict is a doctor line, in both languages: support reads it together with the
	// rest of the node's state.
	code, out, errs = runCLI(nil, "doctor", "--data", dir, "--link", link, "--lang", "ru")
	if !strings.Contains(out, "Ссылка в порядке") {
		t.Fatalf("doctor does not show the link verdict: %d %q %q", code, out, errs)
	}
	code, out, _ = runCLI(nil, "doctor", "--data", dir, "--link", "echofog://join?v=3&card=AAAA", "--lang", "ru")
	if !strings.Contains(out, "другой версией") {
		t.Fatalf("doctor does not name the version problem: %d %q", code, out)
	}
}
