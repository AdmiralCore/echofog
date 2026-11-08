package scenarios

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

// The live mode cannot run here (no Docker), but what it will start is checked statically: every
// compose service a scenario names exists, the new ones stay out of the v1 scenarios' `up -d` (they
// sit in the v2 profile), and the entrypoint has the mode they use.
type composeFile struct {
	Services map[string]struct {
		Profiles    []string          `yaml:"profiles"`
		Environment map[string]string `yaml:"environment"`
		Networks    map[string]any    `yaml:"networks"`
	} `yaml:"services"`
}

func loadCompose(t *testing.T) composeFile {
	t.Helper()
	raw, err := os.ReadFile("../docker-compose.yml")
	require.NoError(t, err)
	var c composeFile
	require.NoError(t, yaml.Unmarshal(raw, &c))
	return c
}

func TestEveryLiveServiceOfEveryScenarioExistsInTheCompose(t *testing.T) {
	c := loadCompose(t)
	for _, s := range Registry() {
		for _, name := range s.Live.Services {
			_, ok := c.Services[name]
			assert.True(t, ok, "%s names the compose service %q, which does not exist", s.Name, name)
		}
	}
}

func TestTheNewV2ServicesAreInTheV2ProfileAndUseTheEfv2Role(t *testing.T) {
	c := loadCompose(t)
	old := map[string]bool{}
	for _, n := range []string{"s1", "s2", "p1", "p2", "x1", "x2", "x3", "h1", "web", "v2a", "v2b", "gw_a", "gw_b", "gw_c",
		"a1", "a2", "b1", "b2", "c1", "c2"} {
		old[n] = true
	}
	found := 0
	for name, svc := range c.Services {
		if old[name] {
			continue
		}
		found++
		assert.Contains(t, svc.Profiles, "v2", "%s is new and must not start with the v1 scenarios", name)
		assert.Equal(t, "efv2", svc.Environment["ROLE"], "%s", name)
		assert.Contains(t, []string{"serve", "idle"}, svc.Environment["EFV2_MODE"], "%s", name)
		if svc.Environment["EFV2_MODE"] == "serve" {
			assert.NotEmpty(t, svc.Environment["EFV2_LISTEN"], "%s serves and needs a listen address", name)
		}
	}
	assert.GreaterOrEqual(t, found, 15, "the services of the six live scenarios")
}

func TestTheEntrypointKnowsTheEfv2Role(t *testing.T) {
	raw, err := os.ReadFile("../entrypoint.sh")
	require.NoError(t, err)
	s := string(raw)
	for _, want := range []string{"efv2)", "EFV2_MODE", "EFV2_LISTEN", "EFV2_PROFILE", "efv2 init", "efv2 run"} {
		assert.True(t, strings.Contains(s, want), "entrypoint.sh has %q", want)
	}
}
