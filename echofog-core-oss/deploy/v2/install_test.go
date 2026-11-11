package v2deploy

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

const installScript = "install.sh"

// pipeToShell finds "| sh" and "| bash" but not "| sha256sum".
var pipeToShell = regexp.MustCompile(`\|\s*(sudo\s+)?(sh|bash|zsh|dash)(\s|$)`)

// The bootstrap script only fetches over https into a private temporary folder and hands the
// verification to the node binary: no curl | sh, no checks done in shell.
func TestInstallScript(t *testing.T) {
	sh := read(t, installScript)
	if !strings.HasPrefix(sh, "#!/bin/sh\n") {
		t.Error("the installer is a POSIX sh script")
	}
	for _, must := range []string{
		"set -eu", "umask 077", "mktemp -d", "trap", "--proto '=https'", "--tlsv1.2", "--proto-redir '=https'", "--connect-timeout", "--max-time", "--fail", "ECHOFOG_RELEASE_URL", "uname -m",
		"v2 setup", "--manifest", "--binary",
		"ssh-keygen", "-Y verify", "BEGIN RELEASE KEYS", "END RELEASE KEYS", "release.txt", "release.txt.sigs", "--allow-rollback", "--fingerprint", "MIN_VERSION_FLOOR", "KEY_EPOCH",
	} {
		if !strings.Contains(sh, must) {
			t.Errorf("install.sh lacks %q", must)
		}
	}
	for _, bad := range []string{"set -x", "eval ", "http://", "--insecure", " -k ", "chmod 777", "sudo "} {
		if strings.Contains(sh, bad) {
			t.Errorf("install.sh contains %q", bad)
		}
	}
	if pipeToShell.MatchString(sh) {
		t.Error("install.sh pipes into a shell")
	}
	// The script must not carry a default download address: the operator names the release.
	if regexp.MustCompile(`https://[a-z0-9.-]+`).MatchString(strings.ReplaceAll(sh, "https://example.org", "")) {
		t.Error("install.sh hardcodes a download address")
	}
	// The downloaded binary is executed once, and that is the setup command, which verifies the
	// signed manifest before it does anything.
	runs := 0
	for _, line := range strings.Split(sh, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `"$BIN"`) {
			runs++
			if !strings.Contains(line, "v2 setup") {
				t.Errorf("the downloaded binary is run other than through v2 setup: %q", line)
			}
		}
	}
	if runs != 1 {
		t.Errorf("the downloaded binary is run %d times, want once", runs)
	}
	// The order is the point of T12.4e: the signature is checked, then the file becomes executable,
	// and nothing from the download is run in between.
	verify, chmod := strings.Index(sh, "verify_release\n"), strings.Index(sh, "chmod 0700")
	if verify < 0 || chmod < 0 || chmod < verify {
		t.Errorf("chmod (%d) must come after the verification call (%d)", chmod, verify)
	}
	if strings.Count(sh, "chmod") != 1 {
		t.Error("the script makes something executable more than once")
	}
	if strings.Contains(sh, "openssl") {
		t.Error("one verification tool only: ssh-keygen")
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
	_, enBody, _ := strings.Cut(en, "\n") // the first line is the link to the Russian version
	if !hasCyr(ru) || hasCyr(enBody) {
		t.Error("language of the guides")
	}
	if !strings.Contains(en, "deploy-vps-v2.ru.md") || !strings.Contains(ru, "deploy-vps-v2.md") {
		t.Error("the guides do not link to each other")
	}
	for name, doc := range map[string]string{"en": en, "ru": ru} {
		for _, must := range []string{"install.sh", "v2 doctor", "v2 status", "v2 link check", "ECHOFOG_RELEASE_URL", "udp", "444/udp", "445/udp", "docker compose", "recovery-phrase.txt", "systemctl"} {
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
		if pipeToShell.MatchString(doc) {
			t.Errorf("%s guide teaches piping a download into a shell", name)
		}
	}
	// T12.4e: the trust root is the key in install.sh, and the guide teaches to compare its
	// fingerprint before the first run. The old "trust anchor" caveat is gone.
	for name, doc := range map[string]string{"en": strings.ToLower(en), "ru": strings.ToLower(ru)} {
		for _, must := range []string{"fingerprint", "sha256sum install.sh", "-o install.sh", "release-trust", "--allow-rollback"} {
			if name == "ru" && must == "fingerprint" {
				must = "отпечат"
			}
			if !strings.Contains(doc, must) {
				t.Errorf("%s guide lacks %q", name, must)
			}
		}
		if strings.Contains(doc, "sed -n") || strings.Contains(doc, "begin release keys") {
			t.Errorf("%s guide teaches to compare the key block only: the whole file is what is compared", name)
		}
		if strings.Contains(doc, "trust anchor") || strings.Contains(doc, "якор") {
			t.Errorf("%s guide still carries the trust-anchor caveat", name)
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

// The release docs: the fingerprint texts for the site and the README, and the key procedures.
func TestReleaseTrustDocs(t *testing.T) {
	en := read(t, "..", "..", "..", "docs", "release-trust.md")
	ru := read(t, "..", "..", "..", "docs", "release-trust.ru.md")
	_, enBody, _ := strings.Cut(en, "\n") // the first line is the link to the Russian version
	if !hasCyr(ru) || hasCyr(enBody) {
		t.Error("language of the release-trust docs")
	}
	for name, doc := range map[string]string{"en": en, "ru": ru} {
		low := strings.ToLower(doc)
		for _, must := range []string{"install.sh", "min_version", "ssh-keygen", "key_epoch", "install-rotation", "install-stamp", "--allow-rollback", "sha256sum install.sh"} {
			if !strings.Contains(low, must) {
				t.Errorf("%s doc lacks %q", name, must)
			}
		}
		// Two independent places: the site page and the repository README.
		if !strings.Contains(low, "site") && !strings.Contains(low, "сайт") {
			t.Errorf("%s doc: no text for the site page", name)
		}
		if !strings.Contains(low, "readme") {
			t.Errorf("%s doc: no text for the README", name)
		}
		if regexp.MustCompile(`[0-9a-fA-F]{64}`).MatchString(doc) || regexp.MustCompile(`AAAA[0-9A-Za-z+/]{20,}`).MatchString(doc) {
			t.Errorf("%s doc contains a real-looking key or fingerprint: the real one is stamped at release", name)
		}
		if regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[a-z]{2,}`).MatchString(doc) {
			t.Errorf("%s doc contains an e-mail address", name)
		}
	}
	if regexp.MustCompile(`(?i)\b(вы|ваш\w*|вам|вас)\b`).MatchString(ru) {
		t.Error("the Russian doc addresses the reader with вы")
	}
	if n := strings.Count(en, "\n#"); n < 5 || n != strings.Count(ru, "\n#") {
		t.Errorf("headings: en %d, ru %d", strings.Count(en, "\n#"), strings.Count(ru, "\n#"))
	}
}
