package servernode

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// UIOptions are what the setup screen needs before it starts.
type UIOptions struct {
	Lang      string
	Advertise string
	DataDir   string
	Port      int
	// Color is false for --plain, NO_COLOR and a non-terminal stdout; the screen then uses no
	// escape sequences at all.
	Color bool
	// Seed is the answer set the flags already gave (--preset ... on the command line).
	Seed SetupAnswers
}

// UIDeps are the side effects of the setup screen.
type UIDeps struct {
	Version string
	Checks  func(ctx context.Context) []CheckResult
	Apply   func(ctx context.Context, plan *SetupPlan, progress func(step string)) (*FinalInfo, error)
}

type setupScreen int

const (
	scrWelcome setupScreen = iota
	scrChecks
	scrPreset
	scrRoles
	scrQuestion
	scrSummary
	scrApplying
	scrDone
	scrPhrase
	scrToken
	scrFailed
)

type question struct {
	id       string
	prompt   string
	hint     string
	def      string
	validate func(string) (string, string)
}

type stepState struct {
	code string
	done bool
}

type applyStepMsg struct{ step string }
type applyDoneMsg struct {
	info *FinalInfo
	err  error
}

type setupModel struct {
	ctx  context.Context
	opts UIOptions
	deps UIDeps

	width, height int
	color         bool
	lang          string
	scr           setupScreen

	answers   SetupAnswers
	presetIdx int

	checks []CheckResult

	qid     string
	input   string
	qerr    string
	carIdx  int
	carSel  map[string]bool
	roleIdx int
	roleSel map[string]bool

	choice int // summary: 0 apply, 1 back, 2 cancel

	steps       []stepState
	events      chan tea.Msg
	final       *FinalInfo
	err         error
	phraseIn    string
	quit        bool
	cancelled   bool
	applied     bool
	applyCancel context.CancelFunc
	stopAsked   bool
}

// SetupTUIResult is what the full-screen setup returns: the plan the operator confirmed, the
// final screen (after a successful apply) and whether the plan is already applied. A caller must
// not apply an applied plan a second time.
type SetupTUIResult struct {
	Plan    *SetupPlan
	Info    *FinalInfo
	Applied bool
}

// SetupTUI is the full-screen setup run by the command; tests replace it to drive the interactive
// branch without a terminal.
var SetupTUI = RunSetupTUI

// RunSetupTUI runs the full-screen setup and returns the plan the operator confirmed. It returns
// ErrSetupCancelled when the operator left before applying; after that point the model applies the
// plan itself and returns it with the node running.
func RunSetupTUI(ctx context.Context, opts UIOptions, deps UIDeps) (*SetupTUIResult, error) {
	m := newSetupModel(ctx, opts, deps)
	p := tea.NewProgram(m, tea.WithAltScreen())
	out, err := p.Run()
	if err != nil {
		return nil, err
	}
	final, ok := out.(*setupModel)
	if !ok {
		return nil, errors.New("servernode: setup screen returned nothing")
	}
	return final.result()
}

// result turns the finished model into the value the command uses. A cancelled run is the only
// one that means "nothing was changed"; a failure after the first change is a failure.
func (m *setupModel) result() (*SetupTUIResult, error) {
	if m.cancelled {
		return nil, ErrSetupCancelled
	}
	if m.err != nil {
		return nil, m.err
	}
	return &SetupTUIResult{Plan: m.plan(), Info: m.final, Applied: m.applied}, nil
}

func newSetupModel(ctx context.Context, opts UIOptions, deps UIDeps) *setupModel {
	if opts.Lang == "" {
		opts.Lang = "en"
	}
	seed := opts.Seed
	if seed.Preset == "" {
		seed = DefaultAnswers(PresetRelay, opts.Lang, opts.Advertise, opts.Port, opts.DataDir)
	}
	if seed.Advertise == "" {
		seed.Advertise = opts.Advertise
	}
	if seed.DataDir == "" {
		seed.DataDir = opts.DataDir
	}
	m := &setupModel{
		ctx: ctx, opts: opts, deps: deps, width: 80, color: opts.Color,
		lang: normLang(opts.Lang), answers: seed,
		carSel: map[string]bool{}, roleSel: map[string]bool{},
	}
	for _, c := range seed.Carriers {
		m.carSel[c] = true
	}
	for _, r := range seed.Roles {
		m.roleSel[r] = true
	}
	m.presetIdx = presetIndex(seed.Preset)
	return m
}

func (m *setupModel) Init() tea.Cmd { return nil }

func (m *setupModel) plan() *SetupPlan {
	p, err := PlanFromAnswers(m.answers)
	if err != nil {
		return nil
	}
	return &p
}

var setupPresets = []Preset{
	PresetRelay, PresetExitFree, PresetHomeEntry, PresetExitPrivate, PresetOwn, PresetExitPaid,
}

func presetIndex(p Preset) int {
	for i, x := range setupPresets {
		if x == p {
			return i
		}
	}
	return 0
}

func (m *setupModel) movePreset(d int) {
	for i := m.presetIdx + d; i >= 0 && i < len(setupPresets); i += d {
		if setupPresets[i] != PresetExitPaid {
			m.presetIdx = i
			return
		}
	}
}

func (m *setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.width < 40 {
			m.width = 40
		}
		return m, nil
	case applyStepMsg:
		for i := range m.steps {
			if m.steps[i].code == msg.step {
				m.steps[i].done = true
			}
			if !m.steps[i].done {
				break
			}
		}
		return m, m.waitEvent()
	case applyDoneMsg:
		if msg.err != nil {
			m.err = msg.err
			m.scr = scrFailed
			return m, nil
		}
		m.final = msg.info
		m.applied = true
		m.scr = scrDone
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *setupModel) waitEvent() tea.Cmd {
	ch := m.events
	if ch == nil {
		return nil
	}
	return func() tea.Msg { return <-ch }
}

func (m *setupModel) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return m.ctrlC()
	}
	switch m.scr {
	case scrWelcome:
		switch msg.Type {
		case tea.KeyLeft, tea.KeyRight, tea.KeyRunes:
			if msg.Type != tea.KeyRunes || len(msg.Runes) == 0 || strings.Contains("lLяЯ", string(msg.Runes)) {
				if m.lang == "ru" {
					m.lang = "en"
				} else {
					m.lang = "ru"
				}
				m.answers.Lang = m.lang
			}
		case tea.KeyEnter:
			m.scr = scrChecks
			if m.deps.Checks != nil {
				m.checks = m.deps.Checks(m.ctx)
			}
		case tea.KeyEsc:
			m.cancelled = true
			return m, tea.Quit
		}
	case scrChecks:
		if msg.Type == tea.KeyEnter {
			m.scr = scrPreset
		}
		if msg.Type == tea.KeyEsc {
			m.scr = scrWelcome
		}
	case scrPreset:
		switch msg.Type {
		case tea.KeyUp:
			m.movePreset(-1)
		case tea.KeyDown:
			m.movePreset(1)
		case tea.KeyEnter:
			m.answers.Preset = setupPresets[m.presetIdx]
			m.answers.Invites = m.answers.Preset == PresetHomeEntry || m.answers.Preset == PresetExitPrivate
			if m.answers.Preset == PresetOwn {
				m.scr = scrRoles
			} else {
				m.startQuestions()
			}
		case tea.KeyEsc:
			m.scr = scrChecks
		}
	case scrRoles:
		switch msg.Type {
		case tea.KeyUp:
			if m.roleIdx > 0 {
				m.roleIdx--
			}
		case tea.KeyDown:
			if m.roleIdx < len(setupRoles)-1 {
				m.roleIdx++
			}
		case tea.KeySpace, tea.KeyRunes:
			if msg.Type == tea.KeySpace || string(msg.Runes) == " " {
				r := setupRoles[m.roleIdx]
				m.roleSel[r] = !m.roleSel[r]
			}
		case tea.KeyEnter:
			var roles []string
			for _, r := range setupRoles {
				if m.roleSel[r] {
					roles = append(roles, r)
				}
			}
			if len(roles) == 0 {
				m.qerr = Message(m.lang, "setup.q.roles-empty")
				return m, nil
			}
			m.answers.Roles = roles
			m.startQuestions()
		case tea.KeyEsc:
			m.scr = scrPreset
		}
	case scrQuestion:
		return m.questionKey(msg)
	case scrSummary:
		switch msg.Type {
		case tea.KeyLeft:
			if m.choice > 0 {
				m.choice--
			}
		case tea.KeyRight:
			if m.choice < 2 {
				m.choice++
			}
		case tea.KeyEnter:
			switch m.choice {
			case 0:
				return m.beginApply()
			case 1:
				m.startQuestions()
			default:
				m.cancelled = true
				return m, tea.Quit
			}
		case tea.KeyEsc:
			m.cancelled = true
			return m, tea.Quit
		}
	case scrApplying:
		// Nothing to do: the progress messages arrive on their own.
	case scrDone:
		if msg.Type == tea.KeyEnter || msg.Type == tea.KeyEsc {
			if m.final != nil && m.final.Phrase != "" {
				m.scr = scrPhrase
			} else {
				return m, tea.Quit
			}
		}
	case scrPhrase:
		return m.phraseKey(msg)
	case scrToken:
		if msg.Type == tea.KeyEnter || msg.Type == tea.KeyEsc {
			return m, tea.Quit
		}
	case scrFailed:
		if msg.Type == tea.KeyEnter || msg.Type == tea.KeyEsc {
			return m, tea.Quit
		}
	}
	return m, nil
}

// ctrlC is honest about the state of the installation: before the first change it cancels
// cleanly; while the apply runs it asks the installer to stop and waits for the rollback; after a
// success or a failure it only quits, because the state is no longer "nothing was changed".
func (m *setupModel) ctrlC() (tea.Model, tea.Cmd) {
	switch m.scr {
	case scrApplying:
		if !m.stopAsked {
			m.stopAsked = true
			if m.applyCancel != nil {
				m.applyCancel()
			}
			m.qerr = Message(m.lang, "setup.apply.stopping")
		}
		// The apply goroutine still sends its result; the reader started by beginApply waits for it.
		return m, nil
	case scrDone, scrPhrase, scrToken, scrFailed:
		return m, tea.Quit
	default:
		m.cancelled = true
		return m, tea.Quit
	}
}

var setupRoles = []string{"relay", "mailbox", "dht-server", "exit"}

// startQuestions begins (or restarts) the question sequence.
func (m *setupModel) startQuestions() {
	m.qid = "advertise"
	m.input = ""
	m.qerr = ""
	m.scr = scrQuestion
	m.prepareQuestion()
}

func (m *setupModel) prepareQuestion() {
	q := m.question()
	if q == nil {
		m.scr = scrSummary
		m.choice = 0
		return
	}
	m.input = ""
	m.qerr = ""
	if q.id == "carriers" {
		if len(m.carSel) == 0 {
			for _, c := range defaultCarriers(m.answers.Domain != "") {
				m.carSel[c] = true
			}
		}
	}
}

func defaultCarriers(hasCover bool) []string {
	if hasCover {
		// The T2.6e server preset: reality2 on the public port; ws2 is offered as an extra and
		// needs a CDN in front of it.
		return []string{"reality2-quic", "reality2-tcp"}
	}
	return []string{"noise3"}
}

func (m *setupModel) question() *question {
	a := &m.answers
	switch m.qid {
	case "advertise":
		return &question{id: "advertise", prompt: Message(m.lang, "setup.q.advertise"), hint: Message(m.lang, "setup.q.advertise.hint"),
			def: a.Advertise, validate: func(s string) (string, string) {
				if checkAdvertise(s) != nil {
					return "", Message(m.lang, "setup.q.invalid-address")
				}
				return s, ""
			}}
	case "port":
		return &question{id: "port", prompt: Message(m.lang, "setup.q.port"), hint: Message(m.lang, "setup.q.port.hint"),
			def: strconv.Itoa(a.Port), validate: func(s string) (string, string) {
				n, err := strconv.Atoi(s)
				if err != nil || n < 1 || n > 65535 {
					return "", Message(m.lang, "setup.q.invalid-port")
				}
				return s, ""
			}}
	case "domain":
		return &question{id: "domain", prompt: Message(m.lang, "setup.q.domain"), hint: Message(m.lang, "setup.q.domain.hint"),
			def: a.Domain, validate: func(s string) (string, string) {
				s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
				if s != "" && !validDomain(s) {
					return "", Message(m.lang, "setup.q.invalid-domain")
				}
				return s, ""
			}}
	case "site-dir":
		return &question{id: "site-dir", prompt: Message(m.lang, "setup.q.site-dir"), hint: Message(m.lang, "setup.q.site-dir.hint"),
			def: a.SiteDir, validate: func(s string) (string, string) {
				s = strings.TrimSpace(s)
				if s == "" {
					return "", Message(m.lang, "setup.q.site-required")
				}
				return s, ""
			}}
	case "cert":
		return &question{id: "cert", prompt: Message(m.lang, "setup.q.cert"), hint: Message(m.lang, "setup.q.cert.hint"),
			def: a.CoverCert, validate: func(s string) (string, string) {
				s = strings.TrimSpace(s)
				if s == "" || strings.EqualFold(s, "acme") {
					return "", ""
				}
				return s, ""
			}}
	case "bandwidth":
		return &question{id: "bandwidth", prompt: Message(m.lang, "setup.q.bandwidth"), hint: Message(m.lang, "setup.q.bandwidth.hint"),
			def: strconv.Itoa(a.BandwidthMbps), validate: func(s string) (string, string) {
				n, err := strconv.Atoi(s)
				if err != nil || n < 0 || n > 100000 {
					return "", Message(m.lang, "setup.q.invalid-number")
				}
				return s, ""
			}}
	case "sessions":
		return &question{id: "sessions", prompt: Message(m.lang, "setup.q.sessions"), hint: Message(m.lang, "setup.q.sessions.hint"),
			def: strconv.Itoa(a.Sessions), validate: func(s string) (string, string) {
				n, err := strconv.Atoi(s)
				if err != nil || n < 0 || n > 10000 {
					return "", Message(m.lang, "setup.q.invalid-number")
				}
				return s, ""
			}}
	case "invites":
		return &question{id: "invites", prompt: Message(m.lang, "setup.q.invites"), hint: Message(m.lang, "setup.q.invites.hint"),
			def: yesNo(a.Invites), validate: m.boolAnswer}
	case "open-ports":
		return &question{id: "open-ports", prompt: Message(m.lang, "setup.q.open-ports"), hint: Message(m.lang, "setup.q.open-ports.hint"),
			def: yesNo(a.OpenPorts), validate: m.boolAnswer}
	case "carriers":
		return &question{id: "carriers", prompt: Message(m.lang, "setup.q.carriers"), hint: Message(m.lang, "setup.q.carriers.hint")}
	}
	return nil
}

func yesNo(v bool) string {
	if v {
		return "y"
	}
	return "n"
}

func (m *setupModel) boolAnswer(s string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "д", "да":
		return "y", ""
	case "n", "no", "н", "нет":
		return "n", ""
	}
	return "", Message(m.lang, "setup.q.invalid-answer")
}

func (m *setupModel) questionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	q := m.question()
	if q == nil {
		m.scr = scrSummary
		return m, nil
	}
	if q.id == "carriers" {
		switch msg.Type {
		case tea.KeyUp:
			if m.carIdx > 0 {
				m.carIdx--
			}
		case tea.KeyDown:
			if m.carIdx < len(setupCarriers)-1 {
				m.carIdx++
			}
		case tea.KeySpace, tea.KeyRunes:
			if msg.Type == tea.KeySpace || string(msg.Runes) == " " {
				c := setupCarriers[m.carIdx]
				m.carSel[c] = !m.carSel[c]
			}
		case tea.KeyEnter:
			var chosen []string
			for _, c := range setupCarriers {
				if m.carSel[c] {
					chosen = append(chosen, c)
				}
			}
			if len(chosen) == 0 {
				m.qerr = Message(m.lang, "setup.q.carriers-empty")
				return m, nil
			}
			if m.answers.Domain == "" {
				for _, c := range chosen {
					if c != "noise3" {
						m.qerr = Message(m.lang, "setup.q.carriers-need-cover")
						return m, nil
					}
				}
			}
			m.answers.Carriers = chosen
			m.advance()
		case tea.KeyEsc:
			m.scr = scrPreset
		}
		return m, nil
	}
	switch msg.Type {
	case tea.KeyRunes:
		m.input += string(msg.Runes)
	case tea.KeySpace:
		m.input += " "
	case tea.KeyBackspace:
		if len(m.input) > 0 {
			m.input = m.input[:len(m.input)-1]
		}
	case tea.KeyEnter:
		ans := strings.TrimSpace(m.input)
		if ans == "" {
			ans = q.def
		}
		val, why := q.validate(ans)
		if why != "" {
			m.qerr = why
			return m, nil
		}
		m.applyAnswer(q.id, val)
		m.advance()
	case tea.KeyEsc:
		m.scr = scrPreset
	}
	return m, nil
}

var setupCarriers = []string{"reality2-quic", "reality2-tcp", "ws2", "noise3"}

func (m *setupModel) applyAnswer(id, val string) {
	a := &m.answers
	switch id {
	case "advertise":
		a.Advertise = val
	case "port":
		a.Port, _ = strconv.Atoi(val)
	case "domain":
		a.Domain = val
	case "site-dir":
		a.SiteDir = val
	case "cert":
		a.CoverCert = val
	case "bandwidth":
		a.BandwidthMbps, _ = strconv.Atoi(val)
	case "sessions":
		a.Sessions, _ = strconv.Atoi(val)
	case "invites":
		a.Invites = val == "y"
	case "open-ports":
		a.OpenPorts = val == "y"
	}
}

func (m *setupModel) advance() {
	var next string
	switch m.qid {
	case "advertise":
		next = "port"
	case "port":
		next = "domain"
	case "domain":
		if m.answers.Domain != "" {
			next = "site-dir"
		} else {
			next = "carriers"
		}
	case "site-dir":
		next = "cert"
	case "cert":
		next = "carriers"
	case "carriers":
		next = "bandwidth"
	case "bandwidth":
		next = "sessions"
	case "sessions":
		next = "invites"
	case "invites":
		next = "open-ports"
	case "open-ports":
		next = ""
	}
	m.qid = next
	if next == "" {
		m.scr = scrSummary
		m.choice = 0
		return
	}
	if next == "carriers" && len(m.carSel) == 0 {
		for _, c := range defaultCarriers(m.answers.Domain != "") {
			m.carSel[c] = true
		}
	}
	m.input = ""
	m.qerr = ""
	if next == "site-dir" && m.answers.SiteDir == "" && m.answers.Preset == PresetExitFree {
		// nothing special: the operator types the folder
	}
}

func (m *setupModel) beginApply() (tea.Model, tea.Cmd) {
	p, err := PlanFromAnswers(m.answers)
	if err != nil {
		m.err = err
		m.scr = scrFailed
		return m, nil
	}
	m.steps = []stepState{
		{"verify", false}, {"user", false}, {"binary", false}, {"profile", false},
		{"unit", false}, {"reload", false}, {"service", false}, {"firewall", false}, {"health", false},
	}
	m.scr = scrApplying
	m.events = make(chan tea.Msg, 16)
	ctx, cancel := context.WithCancel(m.ctx)
	m.applyCancel = cancel
	plan := &p
	go func() {
		defer cancel()
		info, err := m.deps.Apply(ctx, plan, func(step string) {
			m.events <- applyStepMsg{step}
		})
		m.events <- applyDoneMsg{info: info, err: err}
	}()
	return m, func() tea.Msg { return <-m.events }
}

func (m *setupModel) phraseKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyRunes:
		m.phraseIn += string(msg.Runes)
	case tea.KeySpace:
		m.phraseIn += " "
	case tea.KeyBackspace:
		if len(m.phraseIn) > 0 {
			m.phraseIn = m.phraseIn[:len(m.phraseIn)-1]
		}
	case tea.KeyEnter:
		switch strings.ToLower(strings.TrimSpace(m.phraseIn)) {
		case "y", "yes", "д", "да":
			m.removePhraseFile()
			m.scr = scrToken
			m.phraseIn = ""
		default:
			m.qerr = Message(m.lang, "setup.phrase.not-confirmed")
		}
	case tea.KeyEsc:
		// The phrase is shown once: leaving it here removes the file as well.
		m.removePhraseFile()
		m.scr = scrToken
	}
	return m, nil
}

func removeRecoveryFile(dataDir string) error {
	return removeFile(recoveryPath(dataDir))
}

// removePhraseFile deletes the phrase where the installer actually wrote it; the wizard answer
// only helps the tests that fill no final info.
func (m *setupModel) removePhraseFile() {
	if m.final == nil {
		return
	}
	dir := m.final.DataDir
	if dir == "" {
		dir = m.answers.DataDir
	}
	_ = removeRecoveryFile(dir)
}

// ---- view

func (m *setupModel) View() string {
	var body []string
	switch m.scr {
	case scrWelcome:
		body = m.viewWelcome()
	case scrChecks:
		body = m.viewChecks()
	case scrPreset:
		body = m.viewPresets()
	case scrRoles:
		body = m.viewRoles()
	case scrQuestion:
		body = m.viewQuestion()
	case scrSummary:
		body = m.viewSummary()
	case scrApplying:
		body = m.viewApplying()
	case scrDone:
		body = m.viewDone()
	case scrPhrase:
		body = m.viewPhrase()
	case scrToken:
		body = m.viewToken()
	case scrFailed:
		body = m.viewFailed()
	}
	raw := strings.Join(body, "\n")
	if m.color {
		// lipgloss wraps and truncates with the terminal's own cell width, escape sequences included.
		return lipgloss.NewStyle().Width(m.width).MaxWidth(m.width).Render(raw) + "\n"
	}
	lines := wrapLines(strings.Split(raw, "\n"), m.width)
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, truncateRunes(l, m.width))
	}
	return strings.Join(out, "\n") + "\n"
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

func (m *setupModel) style() (accent, dim lipgloss.Style) {
	if !m.color {
		return lipgloss.NewStyle(), lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "25", Dark: "39"}),
		lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "245"})
}

const logo = `  ______      __  ______
 / ____/___  / /_/ ____/___  ____ _
/ __/ / __ \/ __/ /   / __ \/ __ ` + "`" + `/
/ /___/ /_/ / /_/ /___/ /_/ / /_/ /
\____/\____/\__/\____/\____/\__, /
                            /____/`

func (m *setupModel) viewWelcome() []string {
	accent, dim := m.style()
	lines := strings.Split(logo, "\n")
	lines = append(lines,
		"",
		accent.Render("EchoFog v2 · "+m.deps.Version),
		dim.Render(strings.Repeat("─", minInt(40, m.width))),
		Message(m.lang, "setup.welcome.lead"),
		"",
		Message(m.lang, "setup.welcome.lang", m.lang),
		"",
		Message(m.lang, "setup.welcome.start"),
	)
	return lines
}

func (m *setupModel) viewChecks() []string {
	accent, dim := m.style()
	lines := []string{accent.Render(Message(m.lang, "setup.checks.title")), dim.Render(Message(m.lang, "setup.checks.subtitle")), ""}
	for _, c := range m.checks {
		mark := markOf(c.Level, m.color)
		lines = append(lines, fmt.Sprintf("%s %s", mark, c.Text(m.lang)))
	}
	lines = append(lines, "", dim.Render(Message(m.lang, "setup.keys")))
	return lines
}

func markOf(l Level, color bool) string {
	mark := "[ok]"
	switch l {
	case LevelInfo:
		mark = "[--]"
	case LevelWarn:
		mark = "[!]"
	case LevelFail:
		mark = "[x]"
	}
	if !color {
		return mark
	}
	style := lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "42"})
	switch l {
	case LevelWarn:
		style = style.Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"})
	case LevelFail:
		style = style.Foreground(lipgloss.AdaptiveColor{Light: "124", Dark: "203"})
	case LevelInfo:
		style = style.Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "245"})
	}
	return style.Render("[" + mark[1:len(mark)-1] + "]")
}

func (m *setupModel) viewPresets() []string {
	accent, dim := m.style()
	lines := []string{accent.Render(Message(m.lang, "setup.presets.title")), dim.Render(Message(m.lang, "setup.presets.hint")), ""}
	for i, p := range setupPresets {
		name := Message(m.lang, "setup.preset."+string(p)+".name")
		desc := Message(m.lang, "setup.preset."+string(p)+".desc")
		extra := Message(m.lang, "setup.preset."+string(p)+".traffic") + " · " + Message(m.lang, "setup.preset."+string(p)+".risk")
		selected := i == m.presetIdx
		prefix := "  "
		if selected {
			prefix = "> "
		}
		card := prefix + name + "\n  " + desc + "\n  " + extra
		if p == PresetExitPaid {
			card = "  " + name + "\n  " + Message(m.lang, "setup.presets.paid")
			card = dim.Render(card)
		} else if selected && m.color {
			card = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.AdaptiveColor{Light: "25", Dark: "39"}).Width(m.width - 4).Render(card)
		} else {
			card = wrapText(card, m.width-2)
		}
		lines = append(lines, card, "")
	}
	lines = append(lines, dim.Render(Message(m.lang, "setup.keys")))
	return lines
}

func (m *setupModel) viewRoles() []string {
	accent, dim := m.style()
	lines := []string{accent.Render(Message(m.lang, "setup.q.roles")), dim.Render(Message(m.lang, "setup.q.roles.hint")), ""}
	for i, r := range setupRoles {
		box := "[ ]"
		if m.roleSel[r] {
			box = "[x]"
		}
		prefix := "  "
		if i == m.roleIdx {
			prefix = "> "
		}
		lines = append(lines, prefix+box+" "+r)
	}
	if m.qerr != "" {
		lines = append(lines, "", m.qerr)
	}
	lines = append(lines, "", dim.Render(Message(m.lang, "setup.roles.keys")))
	return lines
}

func (m *setupModel) viewQuestion() []string {
	accent, dim := m.style()
	q := m.question()
	if q == nil {
		return []string{Message(m.lang, "setup.q.done")}
	}
	lines := []string{accent.Render(q.prompt), dim.Render(q.hint), ""}
	if q.id == "carriers" {
		for i, c := range setupCarriers {
			box := "[ ]"
			if m.carSel[c] {
				box = "[x]"
			}
			prefix := "  "
			if i == m.carIdx {
				prefix = "> "
			}
			lines = append(lines, prefix+box+" "+c)
		}
	} else {
		def := q.def
		if def == "" {
			def = Message(m.lang, "wizard.none")
		}
		shown := m.input
		lines = append(lines, "> "+shown+"_", dim.Render(Message(m.lang, "setup.q.default", def)))
	}
	if m.qerr != "" {
		lines = append(lines, "", m.qerr)
	}
	lines = append(lines, "", dim.Render(Message(m.lang, "setup.keys")))
	return lines
}

func (m *setupModel) viewSummary() []string {
	accent, dim := m.style()
	p, err := PlanFromAnswers(m.answers)
	lines := []string{accent.Render(Message(m.lang, "setup.summary.title"))}
	if err != nil {
		lines = append(lines, err.Error())
		return lines
	}
	lines = append(lines, SetupSummary(p, m.width)...)
	lines = append(lines, "")
	labels := []string{Message(m.lang, "setup.summary.apply"), Message(m.lang, "setup.summary.back"), Message(m.lang, "setup.summary.cancel")}
	var bar []string
	for i, l := range labels {
		if i == m.choice {
			bar = append(bar, accent.Render("> "+l+" "))
		} else {
			bar = append(bar, "  "+l+" ")
		}
	}
	lines = append(lines, strings.Join(bar, " "), "", dim.Render(Message(m.lang, "setup.keys")))
	return lines
}

func (m *setupModel) viewApplying() []string {
	accent, dim := m.style()
	lines := []string{accent.Render(Message(m.lang, "setup.apply.title")), ""}
	for _, s := range m.steps {
		mark := "[ ]"
		if s.done {
			mark = "[ok]"
		}
		lines = append(lines, mark+" "+Message(m.lang, "setup.apply.step."+s.code))
	}
	if m.qerr != "" {
		lines = append(lines, "", m.qerr)
	}
	lines = append(lines, "", dim.Render(Message(m.lang, "setup.apply.hint")))
	return lines
}

func (m *setupModel) viewDone() []string {
	accent, dim := m.style()
	lines := []string{accent.Render(Message(m.lang, "setup.done.title"))}
	if m.final == nil {
		return lines
	}
	if m.final.PublicLink != "" {
		lines = append(lines, Message(m.lang, "setup.done.link", m.final.PublicLink))
		if q := qrText(m.final.PublicLink, m.width); len(q) > 0 {
			lines = append(lines, q...)
		}
	} else {
		lines = append(lines, Message(m.lang, "setup.done.no-link"))
	}
	if m.final.InviteLink != "" {
		lines = append(lines, Message(m.lang, "setup.done.invite", m.final.InviteLink))
	}
	if m.final.CoverCertDays > 0 {
		lines = append(lines, Message(m.lang, "setup.done.cert", m.final.CoverCertDays))
	}
	for _, w := range m.final.Warnings {
		lines = append(lines, Message(m.lang, "setup.done.warning", w))
	}
	lines = append(lines, "", dim.Render(Message(m.lang, "setup.done.next")))
	return lines
}

func (m *setupModel) viewPhrase() []string {
	accent, dim := m.style()
	lines := []string{accent.Render(Message(m.lang, "setup.phrase.title")), dim.Render(Message(m.lang, "setup.phrase.hint")), ""}
	if m.final != nil {
		lines = append(lines, strings.Split(wrapText(m.final.Phrase, m.width), "\n")...)
	}
	lines = append(lines, "", Message(m.lang, "setup.phrase.confirm"), "> "+m.phraseIn+"_")
	if m.qerr != "" {
		lines = append(lines, "", m.qerr)
	}
	return lines
}

func (m *setupModel) viewToken() []string {
	accent, dim := m.style()
	lines := []string{accent.Render(Message(m.lang, "setup.token.title")), dim.Render(Message(m.lang, "setup.token.hint", tokenFileName)), ""}
	if m.final != nil {
		lines = append(lines, m.final.Token)
	}
	lines = append(lines, "", Message(m.lang, "setup.token.file", tokenFileName), "", dim.Render(Message(m.lang, "setup.token.exit")))
	return lines
}

func (m *setupModel) viewFailed() []string {
	accent, _ := m.style()
	lines := []string{accent.Render(Message(m.lang, "setup.failed.title")), ""}
	if m.err != nil {
		lines = append(lines, strings.Split(wrapText(m.err.Error(), m.width), "\n")...)
	}
	lines = append(lines, "", Message(m.lang, "setup.failed.rolled-back"), Message(m.lang, "setup.failed.exit"))
	return lines
}

// ---- text helpers (also used by the plain summary)

// wrapText wraps a string on spaces to width, keeping existing newlines.
func wrapText(s string, width int) string {
	return strings.Join(wrapLines(strings.Split(s, "\n"), width), "\n")
}

func wrapLines(lines []string, width int) []string {
	if width < 20 {
		width = 20
	}
	var out []string
	for _, line := range lines {
		if len([]rune(line)) <= width {
			out = append(out, line)
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		words := strings.Fields(strings.TrimLeft(line, " \t"))
		cur := indent
		for _, w := range words {
			sep := ""
			if cur != indent {
				sep = " "
			}
			if len([]rune(cur))+len([]rune(sep))+len([]rune(w)) <= width {
				cur += sep + w
				continue
			}
			out = append(out, cur)
			cur = indent + w
		}
		if cur != indent || len(words) == 0 {
			out = append(out, cur)
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
