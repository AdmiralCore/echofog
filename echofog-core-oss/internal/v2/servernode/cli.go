package servernode

import (
	"bytes"
	"context"
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
)

const usageText = `echofog-node v2 — EchoFog v2 server node
usage:
  echofog-node v2 init   --data DIR --listen noise3=BIND:PORT [--listen ...] --advertise IP
                         [--invite-listen noise3=BIND:PORT] [--roles relay,mailbox,dht-server,exit] [--cover HOST:PORT]
  echofog-node v2 run    --data DIR
  echofog-node v2 status --data DIR
  echofog-node v2 invite create --data DIR [--access 30d] [--link 7d] [--max-entries N]
  echofog-node v2 invite list   --data DIR
  echofog-node v2 invite extend --data DIR ID --access 7d
  echofog-node v2 rotate-keys   --data DIR

init prints the recovery phrase once. Write it down and keep it offline.
The commands other than init and run talk to the running node through its operator API on
127.0.0.1 with the token in DIR/operator.token. Every flag may also be set by the environment:
--data is ECHOFOG_DATA_DIR.
`

type usageError struct{ error }

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

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
			if !strings.Contains(a, "=") {
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

// Main runs an `echofog-node v2` command and returns the exit code: 0 done, 1 failed, 2 misuse.
func Main(args []string, env func(string) string, stdout, stderr io.Writer) int {
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
		err = cmdInit(args[1:], env, stdout)
	case "run":
		err = cmdRun(args[1:], env, stdout)
	case "status":
		err = cmdStatus(args[1:], env, stdout)
	case "invite":
		err = cmdInvite(args[1:], env, stdout)
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

func cmdInit(args []string, env func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	data := dataFlag(fs, env)
	var listens listFlag
	fs.Var(&listens, "listen", "suite=BIND:PORT, repeatable")
	advertise := fs.String("advertise", "", "IP literal the card advertises")
	invite := fs.String("invite-listen", "", "suite=BIND:PORT of the invitation listener")
	roles := fs.String("roles", "", "comma-separated roles")
	cover := fs.String("cover", "", "HOST:PORT of the site the camouflaged suites imitate")
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
	cfg := DefaultConfig()
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "roles" { // given, even empty: --roles "" switches every role off
			cfg.Roles = []string{}
			if *roles != "" {
				cfg.Roles = strings.Split(*roles, ",")
			}
		}
	})
	cfg.Cover = *cover
	spec := func(v string) (ListenSpec, error) {
		suite, bind, ok := strings.Cut(v, "=")
		if !ok {
			return ListenSpec{}, fmt.Errorf("%w: listener %q: want suite=host:port", ErrConfig, v)
		}
		return ListenSpec{Suite: strings.TrimSpace(suite), Bind: strings.TrimSpace(bind), Advertise: *advertise}, nil
	}
	for _, l := range listens {
		s, err := spec(l)
		if err != nil {
			return err
		}
		cfg.Listen = append(cfg.Listen, s)
	}
	if *invite != "" {
		s, err := spec(*invite)
		if err != nil {
			return err
		}
		cfg.InviteListen = &s
	}
	phrase, err := Init(*data, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "recovery phrase (shown once; write it down and keep it offline):\n%s\n\ndata:     %s\nroles:    %s\noperator: %s (token in %s)\n",
		phrase, *data, strings.Join(cfg.Roles, ","), cfg.Operator.Addr, filepath.Join(*data, tokenFileName))
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

func cmdStatus(args []string, env func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
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
	var st StatusInfo
	if err := c.call("GET", "/v1/status", nil, &st); err != nil {
		return err
	}
	fmt.Fprintf(out, "node id:     %s\ndevice id:   %s\nroles:       %s\nsessions:    %d\npublic card: %v\n",
		st.NodeID, st.DeviceID, strings.Join(st.Roles, ","), st.Sessions, st.PublicCard)
	for _, l := range st.Listen {
		fmt.Fprintln(out, "listen:     ", l)
	}
	for _, w := range st.Warnings {
		fmt.Fprintln(out, "warning:    ", w)
	}
	return nil
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
	_, pos, err := clientFor(fs, data, rest)
	if err != nil {
		return err
	}
	// Terms are checked before any request.
	for _, t := range []string{*accessTerm, *linkTerm} {
		if t == "" {
			continue
		}
		if _, err := ParseDuration(t); err != nil {
			return usageError{err}
		}
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
