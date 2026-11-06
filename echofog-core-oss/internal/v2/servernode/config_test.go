package servernode_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/servernode"
)

const goodConfig = `{
  "version": 1,
  "listen": [{"suite": "noise3", "bind": "0.0.0.0:443", "advertise": "203.0.113.10"}],
  "roles": ["relay", "exit"],
  "operator": {"addr": "127.0.0.1:0"}
}`

func TestParseConfigGood(t *testing.T) {
	c, err := servernode.ParseConfig([]byte(goodConfig))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(c.Listen) != 1 || c.Listen[0].Advertise != "203.0.113.10" || len(c.Roles) != 2 {
		t.Fatalf("config %+v", c)
	}
	d := servernode.DefaultConfig()
	if err := d.Validate(); err == nil {
		t.Fatal("a default config without a listener must not validate: a server needs an address")
	}
	// Roles are chosen by the profile: the default is the relay/mailbox/dht server, never an exit.
	for _, r := range d.Roles {
		if r == "exit" {
			t.Fatal("exit is on by default")
		}
	}
	// A loopback advertise address is allowed (a node next to a desktop on the same computer).
	local := strings.Replace(goodConfig, "203.0.113.10", "127.0.0.1", 1)
	if _, err := servernode.ParseConfig([]byte(local)); err != nil {
		t.Fatalf("loopback advertise: %v", err)
	}
}

func TestParseConfigRefusals(t *testing.T) {
	with := func(old, new string) string { return strings.Replace(goodConfig, old, new, 1) }
	tests := map[string]string{
		"empty":                  ``,
		"not json":               `listen: x`,
		"unknown field":          with(`"roles"`, `"debug_addr": "0.0.0.0:6060", "roles"`),
		"trailing document":      goodConfig + goodConfig,
		"wrong version":          with(`"version": 1`, `"version": 2`),
		"no listener":            with(`[{"suite": "noise3", "bind": "0.0.0.0:443", "advertise": "203.0.113.10"}]`, `[]`),
		"reality2 not served":    with(`"noise3"`, `"reality2-quic"`),
		"unknown suite":          with(`"noise3"`, `"quic"`),
		"port 0":                 with(`0.0.0.0:443`, `0.0.0.0:0`),
		"bind without port":      with(`0.0.0.0:443`, `0.0.0.0`),
		"advertise hostname":     with(`203.0.113.10`, `example.com`),
		"advertise unspecified":  with(`203.0.113.10`, `0.0.0.0`),
		"advertise multicast":    with(`203.0.113.10`, `224.0.0.1`),
		"unknown role":           with(`"exit"`, `"admin"`),
		"duplicate role":         with(`"exit"`, `"relay"`),
		"operator on all ifaces": with(`127.0.0.1:0`, `0.0.0.0:8080`),
		"operator v6 any":        with(`127.0.0.1:0`, `[::]:8080`),
		"operator hostname":      with(`127.0.0.1:0`, `localhost:8080`),
		"operator LAN":           with(`127.0.0.1:0`, `192.168.1.5:8080`),
		"operator no port":       with(`127.0.0.1:0`, `127.0.0.1`),
		"cover not host:port":    with(`"roles"`, `"cover": "just-a-name", "roles"`),
		"huge":                   goodConfig[:len(goodConfig)-2] + `, "cover": "` + strings.Repeat("a", servernode.MaxConfigSize) + `:443"}`,
	}
	for name, in := range tests {
		if _, err := servernode.ParseConfig([]byte(in)); !errors.Is(err, servernode.ErrConfig) {
			t.Errorf("%s: err = %v, want ErrConfig", name, err)
		}
	}
	// Too many listeners.
	var ls []string
	for i := 0; i < 9; i++ {
		ls = append(ls, fmt.Sprintf(`{"suite":"noise3","bind":"0.0.0.0:%d","advertise":"203.0.113.10"}`, 4000+i))
	}
	many := with(`[{"suite": "noise3", "bind": "0.0.0.0:443", "advertise": "203.0.113.10"}]`, "["+strings.Join(ls, ",")+"]")
	if _, err := servernode.ParseConfig([]byte(many)); !errors.Is(err, servernode.ErrConfig) {
		t.Errorf("9 listeners: %v", err)
	}
	// The same bind twice, and an invitation listener on the main bind.
	twice := with(`[{"suite": "noise3", "bind": "0.0.0.0:443", "advertise": "203.0.113.10"}]`,
		`[{"suite":"noise3","bind":"0.0.0.0:443","advertise":"203.0.113.10"},{"suite":"noise3","bind":"0.0.0.0:443","advertise":"203.0.113.10"}]`)
	if _, err := servernode.ParseConfig([]byte(twice)); !errors.Is(err, servernode.ErrConfig) {
		t.Errorf("duplicate listener: %v", err)
	}
	dup := with(`"roles"`, `"invite_listen": {"suite":"noise3","bind":"0.0.0.0:443","advertise":"203.0.113.10"}, "roles"`)
	if _, err := servernode.ParseConfig([]byte(dup)); !errors.Is(err, servernode.ErrConfig) {
		t.Errorf("invite listener on the main bind: %v", err)
	}
	ok := with(`"roles"`, `"invite_listen": {"suite":"noise3","bind":"0.0.0.0:8443","advertise":"203.0.113.10"}, "roles"`)
	if _, err := servernode.ParseConfig([]byte(ok)); err != nil {
		t.Errorf("a separate invite listener: %v", err)
	}
}

func TestParseDuration(t *testing.T) {
	good := map[string]time.Duration{
		"30d": 30 * 24 * time.Hour, "12h": 12 * time.Hour, "90m": 90 * time.Minute,
		"1d12h": 36 * time.Hour, "7d": 7 * 24 * time.Hour,
	}
	for in, want := range good {
		if got, err := servernode.ParseDuration(in); err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "0d", "-1d", "d", "1w", "1.5d", "30", "1d-1h", "99999999999d", "1d1d", " 30d"} {
		if _, err := servernode.ParseDuration(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func FuzzParseConfig(f *testing.F) {
	f.Add([]byte(goodConfig))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"version":1,"listen":[{"suite":"noise3","bind":"[::1]:1","advertise":"::1"}],"operator":{"addr":"[::1]:9"}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := servernode.ParseConfig(b)
		if err != nil {
			if !errors.Is(err, servernode.ErrConfig) {
				t.Fatalf("error %v is not ErrConfig", err)
			}
			return
		}
		if len(b) > servernode.MaxConfigSize {
			t.Fatal("accepted an oversized file")
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("parsed config does not validate: %v", err)
		}
		if !strings.HasPrefix(c.Operator.Addr, "127.") && !strings.HasPrefix(c.Operator.Addr, "[::1]") {
			t.Fatalf("operator API on %q", c.Operator.Addr)
		}
	})
}

func FuzzParseDuration(f *testing.F) {
	for _, s := range []string{"30d", "1d12h", "", "d", "99999999999999999999d"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := servernode.ParseDuration(s)
		if err == nil && (d <= 0 || d > 10*365*24*time.Hour) {
			t.Fatalf("%q = %v", s, d)
		}
	})
}
