package servernode_test

import (
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/servernode"
)

// The setup checks use message codes with the "setup." prefix; the screen must show those texts,
// not an empty line behind the level mark.
func TestSystemCheckTextUsesSetupMessages(t *testing.T) {
	c := servernode.CheckResult{ID: "root", Level: servernode.LevelOK, Code: "setup.check.root.ok"}
	for _, lang := range []string{"ru", "en"} {
		if !strings.Contains(strings.ToLower(c.Text(lang)), "root") {
			t.Errorf("%s: the setup check text is %q", lang, c.Text(lang))
		}
	}
}
