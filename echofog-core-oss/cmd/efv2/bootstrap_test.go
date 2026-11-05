package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

func TestLoadBootstrapFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mask := strings.Repeat("02", 32)
	if _, err := loadBootstrapFile(write("ok", "# list\nnoise3://192.0.2.10:4430?mask="+mask+"\n"), time.Now()); err != nil {
		t.Fatalf("a good list: %v", err)
	}
	for name, body := range map[string]string{
		"empty": "# nothing\n",
		"bad":   "192.0.2.10:4430\n",
		"huge":  strings.Repeat("#", 70<<10),
	} {
		if _, err := loadBootstrapFile(write(name, body), time.Now()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := loadBootstrapFile(filepath.Join(dir, "missing"), time.Now()); err == nil {
		t.Error("a missing file was accepted")
	}
}

// TestBootstrapListJoinsThroughCardEntry starts a listening profile and a client whose list names
// the listener by its card: the client ends up with an authenticated session, and the listener is
// has an authenticated session to it.
func TestBootstrapListJoinsThroughCardEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("starts two stores")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	serverDir, clientDir := t.TempDir(), t.TempDir()
	if _, err := createIdentity(serverDir, "server"); err != nil {
		t.Fatal(err)
	}
	if _, err := createIdentity(clientDir, "desktop"); err != nil {
		t.Fatal(err)
	}
	port := freeUDPPort(t)
	srv, err := openApp(ctx, serverDir, []listenSpec{{
		Suite: objects.SuiteNoise3, Bind: fmt.Sprintf("127.0.0.1:%d", port), AdvertiseHost: "127.0.0.1", Port: uint32(port),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close(ctx)
	if err := srv.rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	env, ok := srv.rt.Cards().Own(objects.ScopePeer)
	if !ok {
		t.Fatal("no server card")
	}
	card, err := objects.VerifyCard(env, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The laboratory listener has no camouflaged address, hence no public card (D37) and no XX or
	// contact link: the list entry is the card itself. XX through the runtime is covered in node.
	entries := []bootstrap.Entry{{Kind: bootstrap.KindCard, Card: card}}

	cli, err := openApp(ctx, clientDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close(ctx)
	if err := cli.rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	rep := runBootstrap(ctx, cli, entries, nopLogger())
	if rep.Joined != 1 || rep.Failed != 0 {
		t.Fatalf("report %+v", rep)
	}
}

func nopLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
