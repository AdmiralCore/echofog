package v2deploy

import (
	"regexp"
	"strings"
	"testing"
)

// The operational document for home entries (H3): both languages, the same structure, dated, no
// owner data, no foreign addresses, and the rules-file process written down.
func TestDomesticEntryGuides(t *testing.T) {
	en := read(t, "..", "..", "..", "docs", "domestic-entry.md")
	ru := read(t, "..", "..", "..", "docs", "domestic-entry.ru.md")
	_, enBody, _ := strings.Cut(en, "\n")
	if !hasCyr(ru) || hasCyr(enBody) {
		t.Error("language of the guides")
	}
	if !strings.Contains(en, "domestic-entry.ru.md") || !strings.Contains(ru, "domestic-entry.md") {
		t.Error("the guides do not link to each other")
	}
	heads := func(s string) int { return len(regexp.MustCompile(`(?m)^#{1,3} `).FindAllString(s, -1)) }
	if heads(en) != heads(ru) || heads(en) < 8 {
		t.Errorf("headings: en %d, ru %d", heads(en), heads(ru))
	}
	for name, doc := range map[string]string{"en": en, "ru": ru} {
		low := strings.ToLower(doc)
		for _, must := range []string{
			"allowed_cidrs", "valid_until", "seq", "efv2-rules", "t15.2a", "access_log off", "home-entry", "install.sh",
			"kz", "by", "am", // neighbouring countries as entry and exit for RU
		} {
			if !strings.Contains(low, must) {
				t.Errorf("%s guide lacks %q", name, must)
			}
		}
		if !regexp.MustCompile(`(?m)^(Data as of|По состоянию на|Данные на): 20\d\d-\d\d-\d\d`).MatchString(doc) {
			t.Errorf("%s guide has no dated data line", name)
		}
		for _, ip := range regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`).FindAllString(doc, -1) {
			if !strings.HasPrefix(ip, "203.0.113.") && !strings.HasPrefix(ip, "198.51.100.") && !strings.HasPrefix(ip, "192.0.2.") && ip != "127.0.0.1" {
				t.Errorf("%s guide: address %s (documentation ranges only: no owner address, no foreign address)", name, ip)
			}
		}
		if regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[a-z]{2,}`).MatchString(doc) || regexp.MustCompile(`[0-9a-fA-F]{40,}`).MatchString(doc) {
			t.Errorf("%s guide has an e-mail address or a key-like string", name)
		}
		// The decisions of H3/H3a in words: curator, offline key, monthly, whole file, no telemetry.
		for _, must := range []string{"offline", "monthly", "telemetry", "30 days"} {
			if name == "en" && !strings.Contains(low, must) {
				t.Errorf("en guide lacks %q", must)
			}
		}
		for _, must := range []string{"офлайн", "раз в месяц", "телеметр", "30 дней"} {
			if name == "ru" && !strings.Contains(low, must) {
				t.Errorf("ru guide lacks %q", must)
			}
		}
	}
	if regexp.MustCompile(`(?i)\b(вы|ваш\w*|вам|вас)\b`).MatchString(ru) {
		t.Error("the Russian guide addresses the reader with вы")
	}
	// The risk section is there and is not legal advice.
	if !strings.Contains(strings.ToLower(en), "not legal advice") || !strings.Contains(strings.ToLower(ru), "не является юридической") {
		t.Error("the risk section must say that it is not legal advice")
	}
}
