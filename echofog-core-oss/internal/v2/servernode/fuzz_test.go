package servernode_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/servernode"
)

// FuzzBuildPreset: any answers give either ErrConfig/ErrPresetUnavailable or a configuration that
// validates and round-trips through the commented file.
func FuzzBuildPreset(f *testing.F) {
	f.Add("relay", "203.0.113.10", 443, "example.org", "/srv/site")
	f.Add("exit-free", "::1", 65535, "me.duckdns.org", "")
	f.Add("home-entry", "203.0.113.10", 65535, "", "")
	f.Fuzz(func(t *testing.T, preset, addr string, port int, domain, site string) {
		cfg, _, err := servernode.BuildPreset(servernode.Preset(preset), servernode.PresetOptions{Advertise: addr, Port: port, Domain: domain, SiteDir: site})
		if err != nil {
			if !errors.Is(err, servernode.ErrConfig) && !errors.Is(err, servernode.ErrPresetUnavailable) {
				t.Fatalf("error %v", err)
			}
			return
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("preset config does not validate: %v", err)
		}
		raw, err := servernode.RenderConfig(cfg, "en")
		if err != nil {
			t.Fatal(err)
		}
		back, err := servernode.ParseConfig(raw)
		if err != nil {
			t.Fatalf("rendered config does not parse: %v\n%s", err, raw)
		}
		if back.Cover != cfg.Cover || back.SiteDir != cfg.SiteDir {
			t.Fatalf("round trip changed the config")
		}
	})
}

// FuzzRunWizard: arbitrary terminal input never panics and never yields an invalid configuration.
func FuzzRunWizard(f *testing.F) {
	f.Add("\n\n\n\n\n\n\n")
	f.Add("ru\n2\nexample.org\n/s\n443\n203.0.113.10\n/d\ny\n")
	f.Add(strings.Repeat("x", 5000))
	f.Fuzz(func(t *testing.T, in string) {
		var out bytes.Buffer
		res, err := servernode.RunWizard(strings.NewReader(in), &out, servernode.WizardDefaults{Lang: "en", Advertise: "203.0.113.10", Port: 443, DataDir: "/d"})
		if err != nil {
			if !errors.Is(err, servernode.ErrWizardInput) && !errors.Is(err, servernode.ErrConfig) {
				t.Fatalf("error %v", err)
			}
			return
		}
		if err := res.Config.Validate(); err != nil {
			t.Fatalf("wizard config does not validate: %v", err)
		}
	})
}
