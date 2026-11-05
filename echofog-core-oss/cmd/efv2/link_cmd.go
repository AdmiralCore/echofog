package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/echofog/echofog-core/internal/v2/links"
)

// cmdLink is `efv2 link`:
//
//	efv2 link --data DIR [--join-host HOST]   the node's contact link (needs a public card)
//	efv2 link open <link>                     offline preview of a link, no profile needed
//	efv2 link join --data DIR <link>          start a session to the node of a link
//
// Invitation links belong to hidden nodes; the laboratory node is not hidden, so it mints none.
func cmdLink(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "open":
			return cmdLinkOpen(args[1:])
		case "join":
			return cmdLinkJoin(args[1:])
		}
	}
	return cmdLinkContact(args)
}

func cmdLinkOpen(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: efv2 link open <link>")
	}
	return linkOpen(os.Stdout, args[0], time.Now())
}

type linkPreviewDoc struct {
	Kind     string `json:"kind"`
	NodeID   string `json:"node_id"`
	DeviceID string `json:"device_id"`
	NotAfter string `json:"not_after"`
}

// linkOpen prints the preview of a link. The access secret of an invitation is not printed.
func linkOpen(w io.Writer, link string, now time.Time) error {
	p, err := links.Open(link, now)
	if err != nil {
		return linkError(err)
	}
	return writeJSON(w, linkPreviewDoc{
		Kind:     p.Kind.String(),
		NodeID:   hex.EncodeToString(p.Node[:]),
		DeviceID: hex.EncodeToString(p.Device[:]),
		NotAfter: p.NotAfter.UTC().Format(time.RFC3339),
	})
}

func linkError(err error) error { return fmt.Errorf("%s (%w)", links.UserMessage(err), err) }

func cmdLinkContact(args []string) error {
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	data := fs.String("data", "", "profile data directory")
	joinHost := fs.String("join-host", "", "host of the join page for the web form")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" {
		return errors.New("--data is required")
	}
	if len(fs.Args()) != 0 {
		return fmt.Errorf("unexpected arguments: %v (see: efv2 link open|join)", fs.Args())
	}
	nf, err := loadNodeFile(*data)
	if err != nil {
		return err
	}
	listeners, err := parseListen(joinListen(nf.Listen), nf.Cover)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := openApp(ctx, *data, listeners, nil)
	if err != nil {
		return err
	}
	defer a.Close(ctx)
	svc, err := links.New(links.Config{Cards: a.builder, Clock: a.clock, JoinHost: *joinHost})
	if err != nil {
		return err
	}
	l, err := svc.ContactLink(ctx)
	if err != nil {
		return linkError(err)
	}
	return writeJSON(os.Stdout, struct {
		Link    string `json:"link"`
		Web     string `json:"web,omitempty"`
		Expires string `json:"expires"`
	}{l.URL, l.Web, l.Expires.UTC().Format(time.RFC3339)})
}

func cmdLinkJoin(args []string) error {
	fs := flag.NewFlagSet("link join", flag.ContinueOnError)
	data := fs.String("data", "", "profile data directory")
	timeout := fs.Duration("timeout", 30*time.Second, "session timeout")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if *data == "" {
		return errors.New("--data is required")
	}
	if len(fs.Args()) != 1 {
		return errors.New("usage: efv2 link join --data DIR <link>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	a, err := openApp(ctx, *data, nil, nil)
	if err != nil {
		return err
	}
	defer a.Close(ctx)
	if err := a.rt.Start(ctx); err != nil {
		return err
	}
	svc, err := links.New(links.Config{Dialer: a.rt, Clock: a.clock})
	if err != nil {
		return err
	}
	peer, err := svc.Redeem(ctx, fs.Args()[0])
	if err != nil {
		return linkError(err)
	}
	return writeJSON(os.Stdout, dialDoc{
		PeerNodeID:   hex.EncodeToString(peer.Node[:]),
		PeerDeviceID: hex.EncodeToString(peer.Device[:]),
		Path:         pathName(peer.Path),
		SID:          hex.EncodeToString(peer.SID[:]),
	})
}
