package udpsock

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestIOSUsesControlMessageFallback pins the platform split for the control-message files. iOS
// satisfies the darwin build tag, so a plain "darwin" tag would give it the ECN path from
// oob_unix.go; decisions.md T2.2 requires iOS to take the fallback without control messages.
func TestIOSUsesControlMessageFallback(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not in PATH")
	}
	cmd := exec.Command("go", "list", "-f", "{{.GoFiles}}", ".")
	cmd.Env = append(os.Environ(), "GOOS=ios", "GOARCH=arm64", "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list for ios: %v", err)
	}
	list := strings.TrimSpace(string(out))
	list = strings.TrimSuffix(strings.TrimPrefix(list, "["), "]")
	files := make(map[string]bool)
	for _, name := range strings.Fields(list) {
		files[name] = true
	}
	if !files["oob_other.go"] {
		t.Errorf("GOOS=ios builds %v, want the oob_other.go fallback", files)
	}
	if files["oob_unix.go"] {
		t.Errorf("GOOS=ios builds oob_unix.go, want the fallback without control messages")
	}
}
