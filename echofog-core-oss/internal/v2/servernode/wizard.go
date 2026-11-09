package servernode

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// ErrWizardInput: the input ended or stayed invalid.
var ErrWizardInput = errors.New("servernode: setup input ended or was invalid")

const maxWizardTries = 5

// WizardDefaults are the answers Enter gives.
type WizardDefaults struct {
	Lang      string
	Advertise string
	Port      int
	DataDir   string
}

// WizardResult is what the wizard decided.
type WizardResult struct {
	Lang    string
	Preset  Preset
	Opts    PresetOptions
	Config  Config
	Notes   []Note
	DataDir string
	Start   bool
}

type wizard struct {
	in   *bufio.Reader
	out  io.Writer
	lang string
}

// ask prints a question with its default and reads one line. validate turns the answer into its
// final form or says why it is refused (a message code, or "" for the generic one).
func (w *wizard) ask(prompt, def string, validate func(string) (string, string)) (string, error) {
	for try := 0; try < maxWizardTries; try++ {
		shown := def
		if shown == "" {
			shown = Message(w.lang, "wizard.none")
		}
		fmt.Fprintf(w.out, "%s [%s]: ", prompt, shown)
		line, err := w.in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(w.out)
			return "", fmt.Errorf("%w: %v", ErrWizardInput, err)
		}
		ans := strings.TrimSpace(line)
		if ans == "" {
			ans = def
		}
		val, why := validate(ans)
		if why == "" {
			return val, nil
		}
		fmt.Fprintln(w.out, Message(w.lang, why))
	}
	return "", fmt.Errorf("%w: too many wrong answers", ErrWizardInput)
}

func anything(s string) (string, string) { return s, "" }

func checkAdvertise(s string) error {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Unmap().IsUnspecified() || a.IsMulticast() || a.Unmap() == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return errors.New("not a unicast IP literal")
	}
	return nil
}

var presetChoices = map[string]Preset{
	"1": PresetRelay, "relay": PresetRelay,
	"2": PresetExitFree, "exit-free": PresetExitFree,
	"3": PresetHomeEntry, "home-entry": PresetHomeEntry,
	"4": PresetExitPaid, "exit-paid": PresetExitPaid,
	"5": PresetExitPrivate, "exit-private": PresetExitPrivate,
}

func normLang(s string) string {
	if strings.HasPrefix(strings.ToLower(s), "ru") {
		return "ru"
	}
	return "en"
}

// RunWizard asks the questions of the setup, one line each; Enter takes the default. It prints
// the notes of the chosen preset in the summary and never asks for a secret.
func RunWizard(in io.Reader, out io.Writer, d WizardDefaults) (*WizardResult, error) {
	w := &wizard{in: bufio.NewReader(in), out: out, lang: normLang(d.Lang)}
	fmt.Fprintln(out, Message(w.lang, "wizard.title"))

	lang, err := w.ask(Message(w.lang, "wizard.lang"), w.lang, func(s string) (string, string) {
		switch strings.ToLower(s) {
		case "ru", "en":
			return strings.ToLower(s), ""
		}
		return "", "wizard.bad-answer"
	})
	if err != nil {
		return nil, err
	}
	w.lang = lang

	choice, err := w.ask(Message(w.lang, "wizard.preset"), "1", func(s string) (string, string) {
		p, ok := presetChoices[strings.ToLower(s)]
		switch {
		case !ok:
			return "", "wizard.bad-answer"
		case p == PresetExitPaid || p == PresetExitPrivate:
			return "", "wizard.preset-unavailable"
		}
		return string(p), ""
	})
	if err != nil {
		return nil, err
	}
	res := &WizardResult{Lang: lang, Preset: Preset(choice)}

	res.Opts.Domain, err = w.ask(Message(w.lang, "wizard.domain"), "", func(s string) (string, string) {
		s = strings.ToLower(strings.TrimSuffix(s, "."))
		if s != "" && !validDomain(s) {
			return "", "wizard.domain-invalid"
		}
		return s, ""
	})
	if err != nil {
		return nil, err
	}
	if res.Opts.Domain != "" {
		if res.Opts.SiteDir, err = w.ask(Message(w.lang, "wizard.site-dir"), "", anything); err != nil {
			return nil, err
		}
	}

	defPort := d.Port
	if defPort == 0 {
		defPort = 443
	}
	portText, err := w.ask(Message(w.lang, "wizard.port"), strconv.Itoa(defPort), func(s string) (string, string) {
		if n, err := strconv.Atoi(s); err != nil || n < 1 || n > 65535 {
			return "", "wizard.port-invalid"
		}
		return s, ""
	})
	if err != nil {
		return nil, err
	}
	res.Opts.Port, _ = strconv.Atoi(portText)

	if res.Opts.Advertise, err = w.ask(Message(w.lang, "wizard.address"), d.Advertise, func(s string) (string, string) {
		if checkAdvertise(s) != nil {
			return "", "wizard.address-invalid"
		}
		return s, ""
	}); err != nil {
		return nil, err
	}
	if res.DataDir, err = w.ask(Message(w.lang, "wizard.data-dir"), d.DataDir, func(s string) (string, string) {
		if s == "" || len(s) > 4096 || strings.ContainsRune(s, 0) {
			return "", "wizard.bad-answer"
		}
		return s, ""
	}); err != nil {
		return nil, err
	}

	res.Config, res.Notes, err = BuildPreset(res.Preset, res.Opts)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(out, Message(w.lang, "wizard.summary"))
	domain := res.Config.Cover
	if domain == "" {
		domain = Message(w.lang, "wizard.none")
	}
	fmt.Fprintln(out, " ", Message(w.lang, "wizard.line-role", string(res.Preset)))
	fmt.Fprintln(out, " ", Message(w.lang, "wizard.line-roles", strings.Join(res.Config.Roles, ", ")))
	fmt.Fprintln(out, " ", Message(w.lang, "wizard.line-domain", domain))
	fmt.Fprintln(out, " ", Message(w.lang, "wizard.line-address", res.Opts.Advertise, res.Opts.Port))
	fmt.Fprintln(out, " ", Message(w.lang, "wizard.line-data", res.DataDir))
	for _, n := range res.Notes {
		fmt.Fprintf(out, "  [%s] %s\n", n.Level, Message(w.lang, n.Code))
	}

	start, err := w.ask(Message(w.lang, "wizard.start"), "y", func(s string) (string, string) {
		switch strings.ToLower(s) {
		case "y", "yes", "д", "да":
			return "y", ""
		case "n", "no", "н", "нет":
			return "n", ""
		}
		return "", "wizard.bad-answer"
	})
	if err != nil {
		return nil, err
	}
	res.Start = start == "y"
	return res, nil
}
