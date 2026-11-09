package servernode

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden screens of the setup")

func testDeps() UIDeps {
	return UIDeps{
		Version: "0.3.0",
		Checks: func(context.Context) []CheckResult {
			return []CheckResult{
				{ID: "root", Level: LevelOK, Code: "setup.check.root.ok"},
				{ID: "systemd", Level: LevelOK, Code: "setup.check.systemd.ok"},
				{ID: "ports", Level: LevelOK, Code: "setup.check.ports.ok", Args: []any{"443"}},
				{ID: "clock", Level: LevelWarn, Code: "setup.check.clock.warn"},
				{ID: "disk", Level: LevelOK, Code: "setup.check.disk.ok", Args: []any{20480}},
				{ID: "ipv6", Level: LevelInfo, Code: "setup.check.ipv6.none"},
				{ID: "firewall", Level: LevelOK, Code: "setup.check.firewall.ok", Args: []any{"ufw"}},
				{ID: "docker", Level: LevelInfo, Code: "setup.check.docker.absent"},
			}
		},
	}
}

func testModel(lang string, width int) *setupModel {
	opts := UIOptions{Lang: lang, Advertise: "203.0.113.10", DataDir: "/var/lib/echofog", Port: 443}
	m := newSetupModel(context.Background(), opts, testDeps())
	m.width = width
	m.lang = lang
	m.answers.Lang = lang
	return m
}

func goldenScreen(m *setupModel) string {
	return m.View()
}

// TestSetupScreensGolden pins the screens at 60, 80 and 120 columns in both languages. Run with
// -update to rewrite the files after a deliberate change.
func TestSetupScreensGolden(t *testing.T) {
	screens := map[string]func(*setupModel){
		"welcome": func(m *setupModel) { m.scr = scrWelcome },
		"checks":  func(m *setupModel) { m.scr = scrChecks; m.checks = testDeps().Checks(context.Background()) },
		"presets": func(m *setupModel) { m.scr = scrPreset },
		"summary": func(m *setupModel) {
			m.answers = DefaultAnswers(PresetExitFree, m.lang, "203.0.113.10", 443, "/var/lib/echofog")
			m.answers.Domain, m.answers.SiteDir = "example.org", "/srv/site"
			m.answers.OpenPorts = true
			m.scr = scrSummary
		},
		"question": func(m *setupModel) {
			m.scr = scrQuestion
			m.qid = "domain"
		},
		"applying": func(m *setupModel) {
			m.scr = scrApplying
			m.steps = []stepState{{"verify", true}, {"user", true}, {"binary", true}, {"profile", false}}
		},
		"done": func(m *setupModel) {
			m.scr = scrDone
			m.final = &FinalInfo{PublicLink: "echofog://public/AQIDBA", InviteLink: "echofog://invite/BAUG", CoverCertDays: 62}
		},
	}
	dir := filepath.Join("testdata", "golden")
	if *updateGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, setup := range screens {
		for _, lang := range []string{"ru", "en"} {
			for _, width := range []int{60, 80, 120} {
				m := testModel(lang, width)
				setup(m)
				got := goldenScreen(m)
				file := filepath.Join(dir, "setup_"+name+"_"+strconv.Itoa(width)+"_"+lang+".txt")
				if *updateGolden {
					if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(file)
				if err != nil {
					t.Fatalf("%s: %v (run go test -run TestSetupScreensGolden -update)", file, err)
				}
				if got != string(want) {
					t.Errorf("%s: the screen changed:\n--- got\n%s\n--- want\n%s", file, got, want)
				}
				for _, line := range strings.Split(got, "\n") {
					if len([]rune(line)) > width {
						t.Errorf("%s: a line of %d runes exceeds %d: %q", file, len([]rune(line)), width, line)
					}
					for _, r := range line {
						if r >= 0x1F000 || (r >= 0x2600 && r <= 0x27BF) {
							t.Errorf("%s: emoji %q in the screen", file, r)
						}
						if unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' {
							t.Errorf("%s: a control rune %U", file, r)
						}
					}
				}
			}
		}
	}
}

func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func lipglossWidth(s string) int { return lipgloss.Width(s) }

func runeKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// TestSetupScreenMatchesFlagPlan drives the screens with Enter (the defaults) and checks that the
// resulting configuration and unit are exactly what the flags of --yes produce.
func TestSetupScreenMatchesFlagPlan(t *testing.T) {
	cases := []struct {
		preset   Preset
		domain   string
		siteDir  string
		roles    []string
		carriers []string
	}{
		{PresetRelay, "", "", nil, nil},
		{PresetExitFree, "example.org", "/srv/site", nil, nil},
		{PresetHomeEntry, "", "", nil, nil},
		{PresetExitPrivate, "", "", nil, nil},
		{PresetOwn, "", "", []string{"relay", "mailbox"}, nil},
	}
	for _, c := range cases {
		t.Run(string(c.preset), func(t *testing.T) {
			// A non-default data folder makes the unit check real: a unit that ignores the plan's
			// folder, or a plan that loses it, no longer matches the flag plan.
			const dataDir = "/srv/echofog"
			m := testModel("en", 80)
			m.answers = DefaultAnswers(c.preset, "en", "203.0.113.10", 443, dataDir)
			m.answers.Domain, m.answers.SiteDir = c.domain, c.siteDir
			m.answers.Roles = c.roles
			m.presetIdx = presetIndex(c.preset)
			for _, r := range c.roles {
				m.roleSel[r] = true
			}
			m.scr = scrPreset
			if _, cmd := m.Update(key(tea.KeyEnter)); cmd != nil {
				t.Fatal("the preset screen started a command")
			}
			if m.scr == scrRoles {
				m.Update(key(tea.KeyEnter))
			}
			for i := 0; m.scr == scrQuestion && i < 20; i++ {
				m.Update(key(tea.KeyEnter))
			}
			if m.scr != scrSummary {
				t.Fatalf("the questions ended on screen %v, not the summary (error %q)", m.scr, m.qerr)
			}
			got := m.plan()
			if got == nil {
				t.Fatalf("no plan from the screen (error %v)", m.qerr)
			}
			answers := DefaultAnswers(c.preset, "en", "203.0.113.10", 443, dataDir)
			answers.Domain, answers.SiteDir = c.domain, c.siteDir
			answers.Roles = c.roles
			want, err := PlanFromAnswers(answers)
			if err != nil {
				t.Fatal(err)
			}
			rawGot, _ := RenderConfig(got.Config, "en")
			rawWant, _ := RenderConfig(want.Config, "en")
			if !bytes.Equal(rawGot, rawWant) {
				t.Errorf("server.json differs:\n--- screen\n%s\n--- flags\n%s", rawGot, rawWant)
			}
			unitGot, err := UnitFile("echofog", got.DataDir, "/usr/local/bin/echofog-node")
			if err != nil {
				t.Fatalf("the screen unit: %v", err)
			}
			unitWant, err := UnitFile("echofog", want.DataDir, "/usr/local/bin/echofog-node")
			if err != nil {
				t.Fatalf("the flag unit: %v", err)
			}
			if unitGot != unitWant {
				t.Errorf("units differ:\n--- screen\n%s\n--- flags\n%s", unitGot, unitWant)
			}
			// The unit is built from the plan, not from the fixed folder.
			for _, line := range []string{
				"ExecStart=/usr/local/bin/echofog-node v2 run --data " + got.DataDir,
				"ReadWritePaths=" + got.DataDir,
			} {
				if !strings.Contains(unitGot, line) {
					t.Errorf("the screen unit does not follow the plan's data folder: %q missing\n%s", line, unitGot)
				}
			}
		})
	}
}

// M5: a check marked [x] really stops the setup: Enter does not move on and the screen says why.
func TestSetupChecksStopOnFailure(t *testing.T) {
	m := testModel("en", 80)
	m.scr = scrChecks
	m.checks = []CheckResult{{ID: "ports", Level: LevelFail, Code: "setup.check.ports.busy", Args: []any{"443"}}}
	m.Update(key(tea.KeyEnter))
	if m.scr != scrChecks {
		t.Fatalf("a failed check let the setup continue to screen %v", m.scr)
	}
	if m.qerr == "" {
		t.Fatal("the setup stopped without explaining why")
	}
	if !strings.Contains(strings.Join(m.viewChecks(), "\n"), m.qerr) {
		t.Error("the screen does not show the explanation")
	}
	// The same screen advances when nothing is marked [x].
	m.checks = []CheckResult{{ID: "root", Level: LevelOK, Code: "setup.check.root.ok"}}
	m.Update(key(tea.KeyEnter))
	if m.scr != scrPreset {
		t.Fatalf("a clean check list did not advance: screen %v", m.scr)
	}
}

// The paid exit is visible but cannot be chosen; the arrows jump over it.
func TestSetupPaidPresetIsVisibleAndDisabled(t *testing.T) {
	m := testModel("en", 80)
	m.scr = scrPreset
	m.presetIdx = presetIndex(PresetOwn)
	m.movePreset(1)
	if m.presetIdx != presetIndex(PresetOwn) {
		t.Fatalf("the paid preset was selected: %v", setupPresets[m.presetIdx])
	}
	while := 0
	for i := 0; i < len(setupPresets) && while < 10; i++ {
		m.movePreset(1)
		while++
	}
	m.presetIdx = presetIndex(PresetOwn)
	m.movePreset(-1)
	if setupPresets[m.presetIdx] == PresetExitPaid {
		t.Fatalf("the paid preset was selected upwards")
	}
	if !strings.Contains(m.View(), Message("en", "setup.presets.paid")) {
		t.Error("the paid preset is not visible")
	}
}

// The phrase screen shows the phrase once and deletes the file after the confirmation.
func TestSetupPhraseScreenRemovesTheFile(t *testing.T) {
	dir := t.TempDir()
	phraseFile := filepath.Join(dir, "recovery-phrase.txt")
	if err := os.WriteFile(phraseFile, []byte("secret words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testModel("en", 80)
	m.answers.DataDir = dir
	m.final = &FinalInfo{Phrase: "one two three four five six seven eight nine ten eleven twelve", Token: "token"}
	m.scr = scrPhrase
	for _, r := range "yes" {
		m.Update(runeKey(string(r)))
	}
	m.Update(key(tea.KeyEnter))
	if m.scr != scrToken {
		t.Fatalf("screen %v, want the token screen", m.scr)
	}
	if _, err := os.Stat(phraseFile); !os.IsNotExist(err) {
		t.Error("the phrase file was not removed")
	}
}

func TestSetupCancelChangesNothing(t *testing.T) {
	m := testModel("en", 80)
	m.scr = scrWelcome
	m.Update(key(tea.KeyEsc))
	if !m.cancelled {
		t.Fatal("Esc on the welcome screen did not cancel")
	}
}

// The colour mode renders through lipgloss at every width and still fits the terminal.
func TestSetupColorScreenFits(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	for _, width := range []int{60, 80, 120} {
		m := testModel("ru", width)
		m.color = true
		m.scr = scrPreset
		out := m.View()
		if !strings.Contains(out, "\x1b[") {
			t.Errorf("%d: the colour mode printed no escape sequence", width)
		}
		for _, line := range strings.Split(out, "\n") {
			if w := lipglossWidth(line); w > width {
				t.Errorf("%d: a line of %d cells: %q", width, w, line)
			}
		}
	}
}
