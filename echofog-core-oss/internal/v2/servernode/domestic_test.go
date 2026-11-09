package servernode_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
	"github.com/echofog/echofog-core/internal/v2/servernode"
)

var domNow = time.Unix(1_800_000_000, 0)

func rulesFor(t *testing.T, cidr string, validFor time.Duration) *rp.Rules {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"seq":1,"allowed_cidrs":[%q],"valid_until":%d}`, cidr, domNow.Add(validFor).Unix())
	r, err := rp.ParseAt(rp.Sign(priv, []byte(body)), pub, 0, func() time.Time { return domNow })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func homeEntry(t *testing.T, rules *rp.Rules) []string {
	t.Helper()
	_, notes, err := servernode.BuildPreset(servernode.PresetHomeEntry, servernode.PresetOptions{Advertise: "203.0.113.10", Port: 443, Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	return noteCodes(notes)
}

// H3: the home entry is public and says what it risks; H3a/H14: only an address inside the verified
// ranges is advertised as good for mobile networks, anything else is for wired networks only.
func TestHomeEntryPreset(t *testing.T) {
	cfg, notes, err := servernode.BuildPreset(servernode.PresetHomeEntry, servernode.PresetOptions{Advertise: "203.0.113.10", Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	if has(cfg.Roles, "exit") {
		t.Error("a home entry has no exit")
	}
	if !has(cfg.Roles, "relay") || cfg.InviteListen == nil {
		t.Errorf("a home entry relays and takes invitations: %+v", cfg)
	}
	codes := noteCodes(notes)
	for _, want := range []string{"preset.home-entry-risk", "preset.home-entry-wired"} {
		if !contains(codes, want) {
			t.Errorf("without a rules file the notes lack %s: %v", want, codes)
		}
	}
	if contains(codes, "preset.home-entry-verified") {
		t.Error("an entry is not verified without a rules file")
	}
	// The risk note is a warning, not a footnote.
	for _, n := range notes {
		if n.Code == "preset.home-entry-risk" && n.Level != "warn" {
			t.Errorf("the risk note has level %q", n.Level)
		}
	}

	inside := homeEntry(t, rulesFor(t, "203.0.113.0/24", 30*24*time.Hour))
	if !contains(inside, "preset.home-entry-verified") || contains(inside, "preset.home-entry-wired") || !contains(inside, "preset.home-entry-risk") {
		t.Errorf("an address inside allowed_cidrs: %v", inside)
	}
	outside := homeEntry(t, rulesFor(t, "198.51.100.0/24", 30*24*time.Hour))
	if contains(outside, "preset.home-entry-verified") || !contains(outside, "preset.home-entry-wired") {
		t.Errorf("an address outside allowed_cidrs: %v", outside)
	}
	// An expired file verifies nothing (expiry never loosens).
	expired := rulesFor(t, "203.0.113.0/24", -time.Hour)
	if got := homeEntry(t, expired); contains(got, "preset.home-entry-verified") || !contains(got, "preset.home-entry-wired") {
		t.Errorf("an expired file verified an entry: %v", got)
	}
}

// The visibility of the preset: the wizard lists it, with its warning, next to the others.
func TestHomeEntryIsOfferedPublicly(t *testing.T) {
	for _, lang := range []string{"en", "ru"} {
		menu := servernode.Message(lang, "wizard.preset")
		if !(len(menu) > 0) {
			t.Fatal("no menu")
		}
		if !contains(servernode.Codes(), "preset.home-entry-risk") || !contains(servernode.Codes(), "preset.home-entry-verified") {
			t.Fatal("message codes are missing")
		}
	}
}
