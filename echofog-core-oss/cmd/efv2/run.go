package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	data := fs.String("data", "", "profile data directory")
	listen := fs.String("listen", "", "noise3=HOST:PORT[,reality2-quic=HOST:PORT]")
	cover := fs.String("cover", "", "cover host:port of reality2-quic (waits for T2.6b)")
	join := fs.String("join", "", "card file of a node to keep a session with (lab: the relay of a NAT test)")
	bootstrapPath := fs.String("bootstrap", "", "bootstrap list file (cards, device IDs and addresses); saved in the profile")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" {
		return errors.New("--data is required")
	}
	if len(fs.Args()) != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	listeners, err := parseListen(*listen, *cover)
	if err != nil {
		return err
	}
	nf, err := loadNodeFile(*data)
	if err != nil {
		return err
	}
	nf.Listen = splitListen(*listen)
	nf.Cover = *cover
	if *bootstrapPath != "" {
		nf.Bootstrap = *bootstrapPath
	}
	var entries []bootstrap.Entry
	if nf.Bootstrap != "" {
		if entries, err = loadBootstrapFile(nf.Bootstrap, time.Now()); err != nil {
			return err
		}
	}
	if err := saveNodeFile(*data, nf); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := openApp(ctx, *data, listeners, log)
	if err != nil {
		return err
	}
	if err := a.rt.Start(ctx); err != nil {
		a.Close(context.Background())
		return err
	}
	if err := a.WriteCards(ctx); err != nil {
		a.Close(context.Background())
		return err
	}
	if err := a.WriteStatus(); err != nil {
		log.Warn("could not write the status snapshot", "err", err)
	}
	startDatagramEcho(ctx, a.rt)
	if *join != "" {
		go joinCard(ctx, a, *join, log)
	}
	if len(entries) > 0 {
		go runBootstrap(ctx, a, entries, log)
	}

	fmt.Fprintf(os.Stdout, "efv2 node ready\nnode id:   %s\ndevice id: %s\nlisten:    %s\ncards:     %s\n",
		a.rt.Self().NodeID, a.rt.Self().DeviceID, listenText(*listen),
		filepath.Join(*data, cardsDirName, "peer.json"))
	go statusLoop(ctx, a, log)

	<-ctx.Done()
	if err := a.WriteStatus(); err != nil {
		log.Warn("could not write the final status snapshot", "err", err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a.Close(stopCtx)
	return nil
}

func statusLoop(ctx context.Context, a *app, log *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.WriteStatus(); err != nil {
				log.Warn("could not write the status snapshot", "err", err)
			}
		}
	}
}

func splitListen(spec string) []string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	parts := strings.Split(spec, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func listenText(spec string) string {
	if strings.TrimSpace(spec) == "" {
		return "none (client only)"
	}
	return spec
}

// joinCard opens a session to the node whose card file is path, retrying until it holds one. A node
// behind a NAT does this to be reachable through a relay.
func joinCard(ctx context.Context, a *app, path string, log *slog.Logger) {
	for ctx.Err() == nil {
		doc, err := readCardFile(path)
		if err == nil {
			var card *objects.Card
			if card, _, err = verifyCardDoc(doc, "", a.clock.Now()); err == nil {
				if _, _, err = a.rt.Session(ctx, node.DeviceID(card.DeviceID), node.DialOpt{Card: card}); err == nil {
					log.Info("joined the node of the card file")
					return
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}
