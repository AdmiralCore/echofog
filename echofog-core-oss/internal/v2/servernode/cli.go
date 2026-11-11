package servernode

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/echofog/echofog-core/internal/v2/links"
)

const usageText = `echofog-node v2 — EchoFog v2 server node
usage:
  echofog-node v2 setup  [--lang ru|en] [--plain]                 full-screen installer
  echofog-node v2 setup  --preset relay|exit-free|home-entry|exit-private|own --advertise IP
                         --binary FILE --manifest FILE [--port N] [--domain NAME] [--site-dir DIR]
                         [--open-ports] [--yes] [--lang ru|en]
  echofog-node v2 init   [--lang ru|en]                    interactive setup (questions in the terminal)
  echofog-node v2 init   --preset relay|exit-free|home-entry|exit-private --advertise IP --data DIR [--port N]
                         [--domain NAME] [--site-dir DIR] [--cover-cert FILE] [--yes] [--lang ru|en]
  echofog-node v2 init   --data DIR --listen noise3=BIND:PORT --advertise IP [--invite-listen ...]
                         [--roles relay,mailbox,dht-server,exit] [--cover HOST:PORT --site-dir DIR]
  echofog-node v2 run    --data DIR
  echofog-node v2 entry  --data DIR                                permanent entry line for your clients
  echofog-node v2 status --data DIR [--json] [--watch [--interval 2s]] [--check-update] [--lang ru|en]
  echofog-node v2 install --binary FILE --manifest FILE [--preset ... --advertise IP]
                         [--port N] [--domain NAME] [--start] [--yes] [--lang ru|en]   (root; verifies before installing)
  echofog-node v2 uninstall [--data DIR] [--purge [--yes]] [--lang ru|en]            (root)
  echofog-node v2 doctor --data DIR [--dns] [--link LINK] [--lang ru|en]
  echofog-node v2 doctor --cover [HOST:PORT] --advertise IP [--site-dir DIR] [--data DIR] [--lang ru|en]
  echofog-node v2 update --manifest-url URL [--check] [--yes] [--lang ru|en]
  echofog-node v2 invite create --data DIR [--access 30d] [--link 7d] [--max-entries N] [--out FILE]
  echofog-node v2 invite list   --data DIR
  echofog-node v2 invite extend --data DIR ID --access 7d
  echofog-node v2 link check LINK                                  offline: does this link open?
  echofog-node v2 rotate-keys   --data DIR

setup prints the recovery phrase once and keeps it in recovery-phrase.txt until the operator
confirms; the plain mode (no terminal, CI, ssh -T) never prints it or the operator token.
status, invite and rotate-keys talk to the running node through its operator API on 127.0.0.1 with
the token in DIR/operator.token. entry reads the profile and needs no running node. doctor sends
nothing out unless --dns is given; with --cover it sends DNS, one TCP probe, one TLS handshake and one
GET to the cover's own name and address, so the cover can be checked before the profile exists.
update installs nothing without a yes from the operator and never goes back to an older version.
Every flag may also be set by the environment: --data is ECHOFOG_DATA_DIR.
`

type usageError struct{ error }

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func isBoolFlag(fs *flag.FlagSet, arg string) bool {
	f := fs.Lookup(strings.TrimLeft(arg, "-"))
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// parse accepts positional arguments before or after the flags (all flags take a value).
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			return nil, flag.ErrHelp
		case strings.HasPrefix(a, "-") && a != "-":
			flags = append(flags, a)
			if !strings.Contains(a, "=") && !isBoolFlag(fs, a) {
				if i+1 >= len(args) {
					return nil, usageError{fmt.Errorf("flag %s needs a value", a)}
				}
				i++
				flags = append(flags, args[i])
			}
		default:
			pos = append(pos, a)
		}
	}
	if err := fs.Parse(flags); err != nil {
		return nil, usageError{err}
	}
	return pos, nil
}

// Main runs an `echofog-node v2` command with the process's standard input.
func Main(args []string, env func(string) string, stdout, stderr io.Writer) int {
	return MainIO(args, env, os.Stdin, stdout, stderr)
}

// MainIO runs an `echofog-node v2` command and returns the exit code: 0 done, 1 failed, 2 misuse.
// stdin answers the questions of the setup wizard.
func MainIO(args []string, env func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if env == nil {
		env = func(string) string { return "" }
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	var err error
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	case "init":
		err = cmdInit(args[1:], env, stdin, stdout)
	case "setup":
		err = cmdSetup(args[1:], env, stdin, stdout, stderr)
	case "install":
		err = cmdInstall(args[1:], env, stdin, stdout)
	case "uninstall":
		err = cmdUninstall(args[1:], env, stdin, stdout)
	case "doctor":
		err = cmdDoctor(args[1:], env, stdout)
	case "update":
		err = cmdUpdate(args[1:], env, stdin, stdout, stderr)
	case "run":
		err = cmdRun(args[1:], env, stdout)
	case "entry":
		err = cmdEntry(args[1:], env, stdout, stderr)
	case "status":
		err = cmdStatus(args[1:], env, stdout)
	case "invite":
		err = cmdInvite(args[1:], env, stdout)
	case "link":
		err = cmdLink(args[1:], stdout)
	case "rotate-keys":
		err = cmdRotate(args[1:], env, stdout)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usageText)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, usageText)
		return 0
	}
	if errors.Is(err, errReported) {
		return 1
	}
	fmt.Fprintln(stderr, "echofog-node v2:", err)
	var ue usageError
	if errors.As(err, &ue) || errors.Is(err, ErrConfig) {
		return 2
	}
	return 1
}

func dataFlag(fs *flag.FlagSet, env func(string) string) *string {
	return fs.String("data", env("ECHOFOG_DATA_DIR"), "profile data directory")
}

func needData(data string) error {
	if data == "" {
		return usageError{errors.New("--data is required")}
	}
	return nil
}

func cmdRun(args []string, env func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	data := dataFlag(fs, env)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{fmt.Errorf("unexpected arguments: %v", pos)}
	}
	if err := needData(*data); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, err := Open(ctx, *data, log)
	if err != nil {
		return err
	}
	if err := s.Start(ctx); err != nil {
		_ = s.Close(context.Background())
		return err
	}
	st := s.Status()
	fmt.Fprintf(out, "echofog-node v2 ready\nnode id:   %s\ndevice id: %s\nroles:     %s\noperator:  %s\n",
		st.NodeID, st.DeviceID, strings.Join(st.Roles, ","), s.OperatorAddr())
	for _, w := range st.Warnings {
		fmt.Fprintln(out, "warning:", w)
	}
	<-ctx.Done()
	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.Close(c)
}

// cmdEntry prints the permanent entry lines of a stopped or running node. It reads the profile
// itself, so the operator can hand out the lines before the node is up. Only the public masking key
// is printed; the private one stays on disk and is wiped with the device. A node without a cover
// (or a hidden one) has no public card, so the line is an address only: the warning on stderr says
// that a person still needs an invitation, while stdout stays a clean list of lines.
func cmdEntry(args []string, env func(string) string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("entry", flag.ContinueOnError)
	data := dataFlag(fs, env)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := needData(*data); err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{fmt.Errorf("unexpected arguments: %v", pos)}
	}
	idf, err := loadIdentity(*data)
	if err != nil {
		return err
	}
	raw, err := readLimited(filepath.Join(*data, configFileName), MaxConfigSize)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s is not a server profile: run `echofog-node v2 init` first", *data)
		}
		return err
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return err
	}
	account, device, err := deriveIdentity(idf)
	if err != nil {
		return err
	}
	defer account.Destroy()
	defer device.Destroy()
	lines := entryLines(cfg, device.Masking().Public(), device.ID())
	if len(lines) == 0 {
		return errors.New("the profile has no noise3 listener: there is no permanent entry line")
	}
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	if cfg.Visibility == "hidden" || cfg.Cover == "" {
		fmt.Fprintln(errOut, "warning: this node has no public card (no cover or hidden), so the line above is an address only:")
		fmt.Fprintln(errOut, "it grants no access by itself. Invite the person with `echofog-node v2 invite create`, or add a cover.")
	}
	return nil
}

// cmdLink checks a link offline: no profile, no network. It is the support tool for "the link does
// not open": the verdict says whether the card is fine, expired, too long for this device's clock,
// damaged, or made by another version. The access secret is never printed.
func cmdLink(args []string, out io.Writer) error {
	if len(args) == 0 {
		return usageError{errors.New("link needs check and a link: echofog-node v2 link check LINK")}
	}
	if args[0] != "check" {
		return usageError{fmt.Errorf("unknown link command %q", args[0])}
	}
	fs := flag.NewFlagSet("link check", flag.ContinueOnError)
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{errors.New("link check needs exactly one link")}
	}
	pv, err := links.Open(strings.TrimSpace(pos[0]), time.Now())
	if err != nil {
		switch {
		case errors.Is(err, links.ErrClock):
			fmt.Fprintln(out, "clock: the time on this device differs from the node's; synchronise the clock and try again")
		case errors.Is(err, links.ErrExpired):
			fmt.Fprintln(out, "expired: the link is no longer valid")
		case errors.Is(err, links.ErrMismatch):
			fmt.Fprintln(out, "damaged: the link and its card do not match")
		case errors.Is(err, links.ErrOtherVersion):
			fmt.Fprintln(out, "other version: the link was made by another version of EchoFog")
		case errors.Is(err, links.ErrNotLink):
			fmt.Fprintln(out, "not a link: this is not an EchoFog link")
		default:
			fmt.Fprintf(out, "error: %v\n", err)
		}
		return errReported
	}
	fmt.Fprintf(out, "ok: %s link to node %s…, valid until %s\n",
		pv.Kind, hex.EncodeToString(pv.Node[:4]), pv.NotAfter.UTC().Format(time.RFC3339))
	return nil
}

// LinkCheckResult is the offline verdict for the doctor: the same classes as `v2 link check`, with
// a message code for both languages. It never carries the access secret.
func LinkCheckResult(text string) CheckResult {
	pv, err := links.Open(strings.TrimSpace(text), time.Now())
	r := CheckResult{ID: "link", Level: LevelOK, Code: "link.ok"}
	switch {
	case err == nil:
		r.Args = []any{pv.NotAfter.UTC().Format(time.RFC3339)}
	case errors.Is(err, links.ErrClock):
		r.Code, r.Level = "link.clock", LevelWarn
	case errors.Is(err, links.ErrExpired):
		r.Code, r.Level = "link.expired", LevelFail
	case errors.Is(err, links.ErrMismatch):
		r.Code, r.Level = "link.damaged", LevelFail
	case errors.Is(err, links.ErrOtherVersion):
		r.Code, r.Level = "link.other-version", LevelFail
	case errors.Is(err, links.ErrNotLink):
		r.Code, r.Level = "link.not-a-link", LevelFail
	default:
		r.Code, r.Level = "link.error", LevelFail
	}
	return r
}

// ---- operator client

type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(dataDir string) (*client, error) {
	addr, err := readLimited(filepath.Join(dataDir, addrFileName), 256)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("the node is not running (no %s in %s): start it with `echofog-node v2 run`", addrFileName, dataDir)
		}
		return nil, err
	}
	tok, err := NewTokenFile(filepath.Join(dataDir, tokenFileName))
	if err != nil {
		return nil, err
	}
	return &client{base: "http://" + strings.TrimSpace(string(addr)), token: tok, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// call sends a request and decodes the answer into out. The error carries only the node's short
// message, never the request.
func (c *client) call(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	u, _ := url.JoinPath(c.base, path)
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("cannot reach the operator API: is the node running?")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct{ Error string }
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("the node answered: %s", e.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func clientFor(fs *flag.FlagSet, data *string, args []string) (*client, []string, error) {
	pos, err := parse(fs, args)
	if err != nil {
		return nil, nil, err
	}
	if err := needData(*data); err != nil {
		return nil, nil, err
	}
	return nil, pos, nil
}

func cmdInvite(args []string, env func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return usageError{errors.New("invite needs create, list or extend")}
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("invite "+sub, flag.ContinueOnError)
	data := dataFlag(fs, env)
	accessTerm := fs.String("access", "", "access term, like 30d")
	linkTerm := fs.String("link", "", "link lifetime, like 7d")
	maxEntries := fs.Int64("max-entries", 0, "how many contacts may use the link")
	outFile := fs.String("out", "", "write the link to this file (invite create)")
	_, pos, err := clientFor(fs, data, rest)
	if err != nil {
		return err
	}
	// Terms are checked before any request. The invitation card lives at most 30 days, so a longer
	// --link would be refused by the node or, worse, promised a date the card cannot reach.
	for _, t := range []string{*accessTerm, *linkTerm} {
		if t == "" {
			continue
		}
		if _, err := ParseDuration(t); err != nil {
			return usageError{err}
		}
	}
	if *linkTerm != "" {
		d, _ := ParseDuration(*linkTerm)
		if d > 30*24*time.Hour {
			return usageError{fmt.Errorf("--link %s is too long: an invitation link lives at most 30 days", *linkTerm)}
		}
	}
	if sub != "create" && *outFile != "" {
		return usageError{errors.New("--out is only for invite create")}
	}
	switch sub {
	case "create":
		if len(pos) != 0 {
			return usageError{fmt.Errorf("unexpected arguments: %v", pos)}
		}
		c, err := newClient(*data)
		if err != nil {
			return err
		}
		var res struct {
			ID          string    `json:"id"`
			Link        string    `json:"link"`
			LinkExpires time.Time `json:"link_expires"`
		}
		if err := c.call("POST", "/v1/invites", map[string]any{"access": *accessTerm, "link": *linkTerm, "max_entries": *maxEntries}, &res); err != nil {
			return err
		}
		fmt.Fprintf(out, "id:           %s\nlink:         %s\nlink expires: %s\n", res.ID, res.Link, res.LinkExpires.Format(time.RFC3339))
		fmt.Fprintln(out, "if the link does not open, check the clock on the device first: `echofog-node v2 link check LINK`")
		if *outFile != "" {
			// The link only: the file is a convenience for the operator, not a record of the
			// invitation. It is private (mode 0600) and replaced whole by writePrivate.
			if err := writePrivate(*outFile, []byte(res.Link)); err != nil {
				return fmt.Errorf("cannot write the link to %s: %w", *outFile, err)
			}
		}
		return nil
	case "list":
		if len(pos) != 0 {
			return usageError{fmt.Errorf("unexpected arguments: %v", pos)}
		}
		c, err := newClient(*data)
		if err != nil {
			return err
		}
		var items []accessItem
		if err := c.call("GET", "/v1/access", nil, &items); err != nil {
			return err
		}
		for _, i := range items {
			fmt.Fprintf(out, "%s  %-10s %-8s access until %s  entries %d/%d\n", i.ID, i.Kind, i.State,
				i.AccessExpires.Format(time.RFC3339), i.Entries, i.MaxEntries)
		}
		if len(items) == 0 {
			fmt.Fprintln(out, "no invitations or passes")
		}
		return nil
	case "extend":
		if len(pos) != 1 {
			return usageError{errors.New("invite extend needs the id")}
		}
		if *accessTerm == "" {
			return usageError{errors.New("invite extend needs --access TERM")}
		}
		c, err := newClient(*data)
		if err != nil {
			return err
		}
		var item accessItem
		if err := c.call("POST", "/v1/access/"+pos[0]+"/extend", map[string]string{"by": *accessTerm}, &item); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s access until %s\n", item.ID, item.AccessExpires.Format(time.RFC3339))
		return nil
	}
	return usageError{fmt.Errorf("unknown invite command %q", sub)}
}

func cmdRotate(args []string, env func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("rotate-keys", flag.ContinueOnError)
	data := dataFlag(fs, env)
	_, pos, err := clientFor(fs, data, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{fmt.Errorf("unexpected arguments: %v", pos)}
	}
	c, err := newClient(*data)
	if err != nil {
		return err
	}
	var res struct {
		Reissued []struct{ ID, Link string } `json:"reissued"`
	}
	if err := c.call("POST", "/v1/rotate-keys", nil, &res); err != nil {
		return err
	}
	fmt.Fprintf(out, "keys rotated; %d invitation links were reissued (hand them to their holders)\n", len(res.Reissued))
	for _, r := range res.Reissued {
		fmt.Fprintf(out, "%s  %s\n", r.ID, r.Link)
	}
	return nil
}
