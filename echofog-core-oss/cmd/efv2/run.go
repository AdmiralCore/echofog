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
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	data := fs.String("data", "", "profile data directory")
	listen := fs.String("listen", "", "noise3=HOST:PORT[,reality2-quic=HOST:PORT]")
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
