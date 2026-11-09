package servernode_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/echofog/echofog-core/internal/v2/servernode"
)

func hasCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// Every message exists in both languages; the Russian one is Russian, the English one is not.
func TestMessagesInBothLanguages(t *testing.T) {
	codes := servernode.Codes()
	if len(codes) < 20 {
		t.Fatalf("%d message codes", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] {
			t.Errorf("code %s twice", c)
		}
		seen[c] = true
		ru, en := servernode.Message("ru", c), servernode.Message("en", c)
		if ru == "" || en == "" {
			t.Errorf("%s: ru %q en %q", c, ru, en)
			continue
		}
		if !hasCyrillic(ru) {
			t.Errorf("%s: the Russian text is not Russian: %q", c, ru)
		}
		if hasCyrillic(en) {
			t.Errorf("%s: the English text has Cyrillic: %q", c, en)
		}
		// Neutral form: no first-person or gendered past forms addressed to the operator.
		if regexp.MustCompile(`(?i)\b(вы|ты|вам|тебе)\b|\b\w+(ла|ёл|ел)\s+(вы|ты)\b`).MatchString(ru) {
			t.Errorf("%s: the Russian text addresses the reader directly: %q", c, ru)
		}
	}
	if servernode.Message("fr", "wizard.title") != servernode.Message("en", "wizard.title") {
		t.Error("an unknown language must fall back to English")
	}
	if servernode.Message("en", "no.such.code") != "" {
		t.Error("an unknown code must give an empty text")
	}
}

func TestPresets(t *testing.T) {
	opts := servernode.PresetOptions{Advertise: "203.0.113.10", Port: 443}
	cfg, notes, err := servernode.BuildPreset(servernode.PresetRelay, opts)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("relay config: %v", err)
	}
	if has(cfg.Roles, "exit") {
		t.Error("the relay preset has an exit")
	}
	if len(noteCodes(notes)) == 0 || !contains(noteCodes(notes), "preset.no-domain") {
		t.Errorf("without a domain the node is for its own people only: %v", notes)
	}

	cfg, notes, err = servernode.BuildPreset(servernode.PresetExitFree, servernode.PresetOptions{Advertise: "203.0.113.10", Port: 4443, Domain: "example.org", SiteDir: "/srv/site"})
	if err != nil {
		t.Fatalf("exit-free: %v", err)
	}
	if !has(cfg.Roles, "exit") || cfg.Cover != "example.org:4443" || cfg.Listen[0].Bind != "0.0.0.0:4443" || cfg.SiteDir != "/srv/site" {
		t.Errorf("exit-free config %+v", cfg)
	}
	if !contains(noteCodes(notes), "preset.exit-responsibility") {
		t.Errorf("sharing a connection needs a word about responsibility: %v", notes)
	}

	cfg, notes, err = servernode.BuildPreset(servernode.PresetHomeEntry, opts)
	if err != nil {
		t.Fatalf("home-entry: %v", err)
	}
	if cfg.InviteListen == nil || cfg.InviteListen.Bind != "0.0.0.0:444" || !contains(noteCodes(notes), "preset.home-entry-wired") {
		t.Errorf("home-entry %+v %v", cfg, notes)
	}

	for _, p := range []servernode.Preset{servernode.PresetExitPaid, servernode.PresetExitPrivate, "nonsense"} {
		if _, _, err := servernode.BuildPreset(p, opts); !errors.Is(err, servernode.ErrPresetUnavailable) {
			t.Errorf("preset %q: %v", p, err)
		}
	}
	for name, o := range map[string]servernode.PresetOptions{
		"no address": {Port: 443}, "hostname address": {Advertise: "example.org", Port: 443}, "port 0": {Advertise: "203.0.113.10"},
		"port 70000": {Advertise: "203.0.113.10", Port: 70000}, "domain with a space": {Advertise: "203.0.113.10", Port: 443, Domain: "a b.example"},
		"domain is an IP": {Advertise: "203.0.113.10", Port: 443, Domain: "203.0.113.10"}, "domain too long": {Advertise: "203.0.113.10", Port: 443, Domain: strings.Repeat("a", 260) + ".org"},
		"site dir with NUL": {Advertise: "203.0.113.10", Port: 443, Domain: "example.org", SiteDir: "a\x00b"},
	} {
		if _, _, err := servernode.BuildPreset(servernode.PresetRelay, o); !errors.Is(err, servernode.ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func has(l []string, s string) bool { return contains(l, s) }
func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
func noteCodes(n []servernode.Note) []string {
	var out []string
	for _, x := range n {
		out = append(out, x.Code)
	}
	return out
}

func TestFreeDDNSIsRecognised(t *testing.T) {
	for _, d := range []string{"myhost.duckdns.org", "x.ddns.net", "node.no-ip.org", "a.b.dyndns.org", "MYHOST.DUCKDNS.ORG", "me.hopto.org"} {
		if !servernode.IsFreeDDNS(d) {
			t.Errorf("%s is free DDNS", d)
		}
	}
	for _, d := range []string{"example.org", "duckdns.org.example.com", "notduckdns.org", "my-site.com"} {
		if servernode.IsFreeDDNS(d) {
			t.Errorf("%s is not free DDNS", d)
		}
	}
	_, notes, err := servernode.BuildPreset(servernode.PresetRelay, servernode.PresetOptions{Advertise: "203.0.113.10", Port: 443, Domain: "me.duckdns.org", SiteDir: "/x"})
	if err != nil || !contains(noteCodes(notes), "preset.ddns") {
		t.Errorf("a free DDNS domain must be warned about: %v %v", notes, err)
	}
	_, _, err = servernode.BuildPreset(servernode.PresetRelay, servernode.PresetOptions{Advertise: "203.0.113.10", Port: 443, Domain: "example.org"})
	if !errors.Is(err, servernode.ErrConfig) {
		t.Errorf("a cover domain without site content must be refused (T2.6e fail-closed): %v", err)
	}
}

func TestRenderConfigHasComments(t *testing.T) {
	cfg, _, err := servernode.BuildPreset(servernode.PresetExitFree, servernode.PresetOptions{Advertise: "203.0.113.10", Port: 443, Domain: "example.org", SiteDir: "/srv/site"})
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range []string{"ru", "en"} {
		raw, err := servernode.RenderConfig(cfg, lang)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(raw), "\n//"); n < 3 {
			t.Errorf("%s: %d comment lines", lang, n)
		}
		back, err := servernode.ParseConfig(raw)
		if err != nil {
			t.Fatalf("%s: the commented file does not parse: %v\n%s", lang, err, raw)
		}
		if back.Cover != cfg.Cover || len(back.Roles) != len(cfg.Roles) || back.SiteDir != cfg.SiteDir {
			t.Errorf("%s: round trip changed the config: %+v", lang, back)
		}
		if hasCyrillic(string(raw)) != (lang == "ru") {
			t.Errorf("%s: language of the comments", lang)
		}
	}
	// Only whole-line comments are comments: a value that contains // is a value.
	c, err := servernode.ParseConfig([]byte("// hi\n" + strings.Replace(goodConfig, `"roles"`, `"site_dir": "/srv/site", "cover": "a//b.example:443", "cover_acme": {"accept_tos": true}, "roles"`, 1)))
	if err != nil || c.Cover != "a//b.example:443" {
		t.Errorf("comment handling: %v %+v", err, c)
	}
}

var wizardDefaults = servernode.WizardDefaults{Lang: "en", Advertise: "203.0.113.10", Port: 443, DataDir: "/var/lib/echofog"}

// Enter to the end gives a working node.
func TestWizardEnterToTheEnd(t *testing.T) {
	var out bytes.Buffer
	res, err := servernode.RunWizard(strings.NewReader(strings.Repeat("\n", 20)), &out, wizardDefaults)
	if err != nil {
		t.Fatalf("RunWizard: %v\n%s", err, out.String())
	}
	if err := res.Config.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	if res.Preset != servernode.PresetRelay || res.Opts.Port != 443 || res.DataDir != wizardDefaults.DataDir || !res.Start {
		t.Fatalf("defaults: %+v", res)
	}
	if hasCyrillic(out.String()) {
		t.Error("the default language is English here")
	}
	if !strings.Contains(out.String(), "203.0.113.10") {
		t.Error("the summary does not show the address")
	}
}

func TestWizardAnswers(t *testing.T) {
	// language ru, preset 2 (exit-free), domain, site dir, port, address, data dir, start: no
	in := "ru\n2\nexample.org\n/srv/site\n8443\n198.51.100.7\n/data\nn\n"
	var out bytes.Buffer
	res, err := servernode.RunWizard(strings.NewReader(in), &out, wizardDefaults)
	if err != nil {
		t.Fatalf("RunWizard: %v\n%s", err, out.String())
	}
	if res.Lang != "ru" || res.Preset != servernode.PresetExitFree || res.Opts.Domain != "example.org" || res.Opts.SiteDir != "/srv/site" ||
		res.Opts.Port != 8443 || res.Opts.Advertise != "198.51.100.7" || res.DataDir != "/data" || res.Start {
		t.Fatalf("result %+v", res)
	}
	if !hasCyrillic(out.String()) {
		t.Error("the wizard did not switch to Russian")
	}
	if !has(res.Config.Roles, "exit") || res.Config.Cover != "example.org:8443" {
		t.Errorf("config %+v", res.Config)
	}
}

func TestWizardAsksAgainOnBadAnswers(t *testing.T) {
	// bad language, an unavailable preset, a bad port, a hostname as the address, then good ones.
	in := "klingon\nen\n4\n9\n1\n\n\n99999\nabc\n443\nexample.org\n203.0.113.10\n\n\n"
	var out bytes.Buffer
	res, err := servernode.RunWizard(strings.NewReader(in), &out, servernode.WizardDefaults{Lang: "en", Port: 443, DataDir: "/d"})
	if err != nil {
		t.Fatalf("RunWizard: %v\n%s", err, out.String())
	}
	if res.Preset != servernode.PresetRelay || res.Opts.Port != 443 || res.Opts.Advertise != "203.0.113.10" {
		t.Fatalf("result %+v\n%s", res, out.String())
	}
	if !strings.Contains(out.String(), servernode.Message("en", "wizard.preset-unavailable")) {
		t.Error("an unavailable preset was not explained")
	}
}

func TestWizardWarnsAboutDDNSAndEmptySite(t *testing.T) {
	in := "en\n2\nme.duckdns.org\n/srv/site\n\n\n\n\n\n"
	var out bytes.Buffer
	if _, err := servernode.RunWizard(strings.NewReader(in), &out, wizardDefaults); err != nil {
		t.Fatalf("RunWizard: %v\n%s", err, out.String())
	}
	// The empty-site note is unreachable now: a cover without site_dir is refused (T2.6e).
	for _, code := range []string{"preset.ddns", "preset.exit-responsibility"} {
		if !strings.Contains(out.String(), servernode.Message("en", code)) {
			t.Errorf("the summary lacks the warning %s", code)
		}
	}
}

func TestWizardInputEnds(t *testing.T) {
	for name, in := range map[string]string{"empty input": "", "ends after the language": "en\n"} {
		if _, err := servernode.RunWizard(strings.NewReader(in), &bytes.Buffer{}, servernode.WizardDefaults{Lang: "en", Port: 443}); !errors.Is(err, servernode.ErrWizardInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// No address anywhere: the wizard cannot invent one.
	_, err := servernode.RunWizard(strings.NewReader(strings.Repeat("\n", 40)), &bytes.Buffer{}, servernode.WizardDefaults{Lang: "en", Port: 443, DataDir: "/d"})
	if !errors.Is(err, servernode.ErrWizardInput) {
		t.Errorf("no address: %v", err)
	}
}

// The CLI: interactive init from stdin, and the same result from flags.
func TestCLIInitWizardAndFlags(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "n1")
	in := "en\n1\n\n4443\n127.0.0.1\n" + dir + "\nn\n"
	var out, errb bytes.Buffer
	code := servernode.MainIO([]string{"init"}, nil, strings.NewReader(in), &out, &errb)
	if code != 0 {
		t.Fatalf("init wizard: %d %s %s", code, out.String(), errb.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "//") {
		t.Error("server.json has no comments")
	}
	if c, err := servernode.ParseConfig(raw); err != nil || c.Listen[0].Bind != "0.0.0.0:4443" {
		t.Fatalf("saved config: %v %+v", err, c)
	}
	if !regexp.MustCompile(`(?m)^([a-z]+( [a-z]+){11})$`).MatchString(out.String()) {
		t.Errorf("no recovery phrase in the output:\n%s", out.String())
	}

	dir2 := filepath.Join(t.TempDir(), "n2")
	code = servernode.MainIO([]string{"init", "--preset", "exit-free", "--domain", "example.org", "--site-dir", "/srv/site", "--port", "4444", "--advertise", "127.0.0.1", "--data", dir2, "--yes", "--lang", "ru"},
		nil, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("init --yes: %d %s", code, errb.String())
	}
	raw, _ = os.ReadFile(filepath.Join(dir2, "server.json"))
	if c, err := servernode.ParseConfig(raw); err != nil || !has(c.Roles, "exit") || c.Cover != "example.org:4444" {
		t.Fatalf("flag config: %v %+v", err, c)
	}
	// Scripts never wait for a question: without --yes and without a terminal, missing answers fail.
	code = servernode.MainIO([]string{"init", "--preset", "relay", "--data", filepath.Join(t.TempDir(), "n3")}, nil, strings.NewReader(""), &out, &errb)
	if code == 0 {
		t.Error("init --preset without --advertise succeeded")
	}
	if code := servernode.MainIO([]string{"init", "--preset", "exit-paid", "--advertise", "127.0.0.1", "--data", filepath.Join(t.TempDir(), "n4"), "--yes"}, nil, strings.NewReader(""), &out, &errb); code == 0 {
		t.Error("an unavailable preset succeeded")
	}
}
