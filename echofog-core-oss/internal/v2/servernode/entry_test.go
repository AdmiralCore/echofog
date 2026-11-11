package servernode_test

import (
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/servernode"
)

// profileKeys reads the device id and the masking private key from the profile on disk. The test
// derives the public half itself, so a command that prints the private half fails.
func profileKeys(t *testing.T, dir string) (deviceID string, maskPrivate []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "server-identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idf struct {
		MaskPrivate string `json:"mask_private"`
		DeviceID    string `json:"device_id"`
	}
	if err := json.Unmarshal(raw, &idf); err != nil {
		t.Fatal(err)
	}
	priv, err := hex.DecodeString(idf.MaskPrivate)
	if err != nil || len(priv) != 32 {
		t.Fatalf("mask_private: %v, %d bytes", err, len(priv))
	}
	return idf.DeviceID, priv
}

func entryLines(t *testing.T, out string) []string {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestCLIEntryParsesAndMatchesProfile(t *testing.T) {
	dir, cfg := profile(t, false)
	code, out, errs := runCLI(nil, "entry", "--data", dir)
	if code != 0 {
		t.Fatalf("entry: %d %q", code, errs)
	}
	lines := entryLines(t, out)
	if len(lines) != 1 {
		t.Fatalf("entry printed %d lines, want 1:\n%s", len(lines), out)
	}
	entries, err := bootstrap.Parse(out, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("bootstrap.Parse(%q): %v", out, err)
	}
	if len(entries) != 1 || entries[0].Kind != bootstrap.KindDevice {
		t.Fatalf("parsed %d entries, want one device entry", len(entries))
	}
	if entries[0].Addr.GetSuite() == 0 || entries[0].Addr.Host == "" {
		t.Fatalf("no address in %q", entries[0].Line)
	}
	if want := cfg.Listen[0].Advertise; entries[0].Addr.Host != want {
		t.Fatalf("host %q, want %q", entries[0].Addr.Host, want)
	}
	wantPort := strings.TrimPrefix(cfg.Listen[0].Bind, "127.0.0.1:")
	if got := fmt.Sprint(entries[0].Addr.Port); got != wantPort {
		t.Fatalf("port %s, want %s", got, wantPort)
	}

	deviceID, priv := profileKeys(t, dir)
	if got := hex.EncodeToString(entries[0].Device[:]); got != deviceID {
		t.Fatalf("device %s, want the profile's %s", got, deviceID)
	}
	// The mask must be the public half. Taking the private bytes (the mutation) fails here and on
	// the output check below.
	key, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Mask.Alg() != keys.X25519 {
		t.Fatalf("mask tag %s, want x25519", entries[0].Mask.Alg())
	}
	if !reflect.DeepEqual(entries[0].Mask.Key(), key.PublicKey().Bytes()) {
		t.Fatalf("mask is not the public key of mask_private")
	}
	if strings.Contains(out, hex.EncodeToString(priv)) {
		t.Fatal("the masking private key was printed")
	}
	if strings.Contains(errs, hex.EncodeToString(priv)) {
		t.Fatal("the masking private key went to stderr")
	}
	// A node without a cover serves invitees only: the line is an address, and stderr says so
	// without polluting the line list on stdout.
	if !strings.Contains(errs, "no public card") || !strings.Contains(errs, "invite create") {
		t.Fatalf("entry does not warn about an invite-only node: %q", errs)
	}
}

// With a cover the node has a public card, and the entry line is the permanent access of the
// operator's own clients: no warning.
func TestCLIEntryDoesNotWarnWithACover(t *testing.T) {
	cfg := servernode.DefaultConfig()
	cfg.Listen = []servernode.ListenSpec{{Suite: "noise3", Bind: fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t)), Advertise: "203.0.113.10"}}
	cfg.Cover = "node.example.org:443"
	cfg.SiteDir = t.TempDir()
	dir := filepath.Join(t.TempDir(), "node")
	if _, err := servernode.Init(dir, cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	code, out, errs := runCLI(nil, "entry", "--data", dir)
	if code != 0 {
		t.Fatalf("entry: %d %q", code, errs)
	}
	if lines := entryLines(t, out); len(lines) != 1 {
		t.Fatalf("entry printed %d lines:\n%s", len(lines), out)
	}
	if strings.Contains(errs, "no public card") {
		t.Fatalf("a covered node warns about invite-only access: %q", errs)
	}
}

func TestCLIEntryOneLinePerNoise3Listener(t *testing.T) {
	cfg := servernode.DefaultConfig()
	cfg.Listen = []servernode.ListenSpec{
		{Suite: "noise3", Bind: fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t)), Advertise: "203.0.113.10"},
		{Suite: "noise3", Bind: fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t)), Advertise: "2001:db8::1"},
	}
	dir := filepath.Join(t.TempDir(), "node")
	if _, err := servernode.Init(dir, cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	code, out, errs := runCLI(nil, "entry", "--data", dir)
	if code != 0 {
		t.Fatalf("entry: %d %q", code, errs)
	}
	lines := entryLines(t, out)
	if len(lines) != 2 {
		t.Fatalf("entry printed %d lines, want 2:\n%s", len(lines), out)
	}
	var hosts []string
	for _, l := range lines {
		u, err := url.Parse(l)
		if err != nil {
			t.Fatalf("not a URL %q: %v", l, err)
		}
		hosts = append(hosts, u.Hostname())
	}
	if want := []string{"203.0.113.10", "2001:db8::1"}; !reflect.DeepEqual(hosts, want) {
		t.Fatalf("hosts %v, want %v", hosts, want)
	}
}

func TestStatusJSONEntries(t *testing.T) {
	dir, _ := profile(t, false)
	startServer(t, dir)

	code, out, errs := runCLI(nil, "entry", "--data", dir)
	if code != 0 {
		t.Fatalf("entry: %d %q", code, errs)
	}
	code, raw, errs := runCLI(nil, "status", "--data", dir, "--json")
	if code != 0 {
		t.Fatalf("status: %d %q", code, errs)
	}
	var st struct {
		NodeID       string   `json:"node_id"`
		DeviceID     string   `json:"device_id"`
		Roles        []string `json:"roles"`
		Entry        string   `json:"entry"`
		Entries      []string `json:"entries"`
		Access       string   `json:"access"`
		Public       bool     `json:"public"`
		InviteListen string   `json:"invite_listen"`
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("status JSON: %v\n%s", err, raw)
	}
	// The old fields stay.
	if st.NodeID == "" || st.DeviceID == "" || len(st.Roles) == 0 {
		t.Fatalf("status lost its old fields: %s", raw)
	}
	want := entryLines(t, out)
	if !reflect.DeepEqual(st.Entries, want) {
		t.Fatalf("status entries %v, want %v", st.Entries, want)
	}
	if st.Entry != want[0] {
		t.Fatalf("status entry %q, want %q", st.Entry, want[0])
	}
	if st.Access != "invite" || st.Public {
		t.Fatalf("access %q public %v, want an invite-only node without a public card", st.Access, st.Public)
	}
}
