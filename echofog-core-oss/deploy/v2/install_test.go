package v2deploy

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

const installScript = "install.sh"

// The bootstrap script only fetches over https into a private temporary folder and hands the
// verification to the node binary: no curl | sh, no checks done in shell.
func TestInstallScript(t *testing.T) {
	sh := read(t, installScript)
	if !strings.HasPrefix(sh, "#!/bin/sh\n") {
		t.Error("the installer is a POSIX sh script")
	}
	for _, must := range []string{
		"set -eu", "umask 077", "mktemp -d", "trap", "--proto '=https'", "--tlsv1.2", "--fail", "ECHOFOG_RELEASE_URL", "uname -m",
		"v2 install", "--manifest", "--binary",
	} {
		if !strings.Contains(sh, must) {
			t.Errorf("install.sh lacks %q", must)
		}
	}
	for _, bad := range []string{"set -x", "eval ", "| sh", "|sh", "| bash", "|bash", "http://", "--insecure", " -k ", "chmod 777", "sudo "} {
		if strings.Contains(sh, bad) {
			t.Errorf("install.sh contains %q", bad)
		}
	}
	// The script must not carry a default download address: the operator names the release.
	if regexp.MustCompile(`https://[a-z0-9.-]+`).MatchString(strings.ReplaceAll(sh, "https://example.org", "")) {
		t.Error("install.sh hardcodes a download address")
	}
	// The downloaded binary is not run before the node binary has verified it: the first execution
	// of the downloaded file is the install command, which checks the signed manifest first.
	first := strings.Index(sh, `"$WORK/echofog-node"`)
	verify := strings.Index(sh, "v2 install")
	if first < 0 || verify < 0 || first < verify-200 {
		t.Error("the downloaded binary must be executed only through `v2 install`")
	}
	if p, err := exec.LookPath("sh"); err == nil {
		if out, err := exec.Command(p, "-n", installScript).CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v\n%s", err, out)
		}
	}
}

func hasCyr(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

func TestVPSGuides(t *testing.T) {
	en := read(t, "..", "..", "..", "docs", "deploy-vps-v2.md")
	ru := read(t, "..", "..", "..", "docs", "deploy-vps-v2.ru.md")
	if !hasCyr(ru) || hasCyr(en) {
		t.Error("language of the guides")
	}
	if !strings.Contains(en, "deploy-vps-v2.ru.md") || !strings.Contains(ru, "deploy-vps-v2.md") {
		t.Error("the guides do not link to each other")
	}
	for name, doc := range map[string]string{"en": en, "ru": ru} {
		for _, must := range []string{"install.sh", "v2 doctor", "v2 status", "ECHOFOG_RELEASE_URL", "udp", "docker compose", "recovery-phrase.txt", "systemctl"} {
			if !strings.Contains(strings.ToLower(doc), strings.ToLower(must)) {
				t.Errorf("%s guide lacks %q", name, must)
			}
		}
		for _, ip := range regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`).FindAllString(doc, -1) {
			if !strings.HasPrefix(ip, "203.0.113.") && !strings.HasPrefix(ip, "198.51.100.") && !strings.HasPrefix(ip, "192.0.2.") && ip != "127.0.0.1" && ip != "0.0.0.0" {
				t.Errorf("%s guide: address %s (only documentation ranges)", name, ip)
			}
		}
		if regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[a-z]{2,}`).MatchString(doc) {
			t.Errorf("%s guide contains an e-mail address", name)
		}
		if regexp.MustCompile(`[0-9a-fA-F]{40,}`).MatchString(doc) {
			t.Errorf("%s guide contains a key-like string", name)
		}
		if strings.Contains(doc, "| sh") || strings.Contains(doc, "|sh") || strings.Contains(doc, "| bash") {
			t.Errorf("%s guide teaches piping a download into a shell", name)
		}
	}
	// Same structure in both languages.
	heads := func(s string) int { return len(regexp.MustCompile(`(?m)^#{1,3} `).FindAllString(s, -1)) }
	if heads(en) != heads(ru) || heads(en) < 6 {
		t.Errorf("headings: en %d, ru %d", heads(en), heads(ru))
	}
	// Neutral Russian: no direct address by "вы" and no gendered past forms of the reader.
	if regexp.MustCompile(`(?i)\b(вы|ваш\w*|вам|вас)\b`).MatchString(ru) {
		t.Error("the Russian guide addresses the reader with вы")
	}
}
