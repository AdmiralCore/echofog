package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

func cmdCard(args []string) error {
	fs := flag.NewFlagSet("card", flag.ContinueOnError)
	data := fs.String("data", "", "profile data directory")
	scopeName := fs.String("scope", "peer", "public or peer")
	listen := fs.String("listen", "", "override the listeners of the last run")
	cover := fs.String("cover", "", "cover host:port of reality2-quic (waits for T2.6b)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" {
		return errors.New("--data is required")
	}
	if len(fs.Args()) != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	scope, err := scopeOf(*scopeName)
	if err != nil {
		return err
	}

	cache := filepath.Join(*data, cardsDirName, scope.String()+".json")
	if *listen == "" && runningLikely(*data) {
		doc, err := readCardFile(cache)
		if err == nil {
			return printJSON(doc)
		}
		return fmt.Errorf("the node is running and %s is missing: %w", cache, err)
	}

	nf, err := loadNodeFile(*data)
	if err != nil {
		return err
	}
	spec, cov := *listen, *cover
	if spec == "" {
		spec = joinListen(nf.Listen)
		cov = nf.Cover
	}
	listeners, err := parseListen(spec, cov)
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

	ic, c, err := a.CurrentCard(ctx, scope)
	if errors.Is(err, cards.ErrNoPublicCard) {
		return fmt.Errorf("no public card: a visible node without a camouflaged listener is not published (wire.md §2.1 D37)")
	}
	if err != nil {
		return err
	}
	doc := newCardDoc(ic, c)
	if err := writeCardFile(cache, doc); err != nil {
		return err
	}
	return printJSON(doc)
}

func scopeOf(name string) (objects.Scope, error) {
	switch name {
	case "public":
		return objects.ScopePublic, nil
	case "peer":
		return objects.ScopePeer, nil
	default:
		return 0, fmt.Errorf("scope %q: want public or peer", name)
	}
}

func joinListen(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

// runningLikely reports a fresh status snapshot. It lets `card` read the exported card instead of
// fighting the running node for the store lock.
func runningLikely(dataDir string) bool {
	fi, err := os.Stat(statusPath(dataDir))
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) < 15*time.Second
}

func printJSON(v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(raw))
	return err
}
