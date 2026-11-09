package servernode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Ctrl+C before the apply is a clean cancellation: nothing has been changed.
func TestSetupCtrlCBeforeApplyCancels(t *testing.T) {
	m := testModel("en", 80)
	m.scr = scrSummary
	m.Update(key(tea.KeyCtrlC))
	if !m.cancelled {
		t.Fatal("Ctrl+C on the summary did not cancel")
	}
	if _, err := m.result(); !errors.Is(err, ErrSetupCancelled) {
		t.Fatalf("the result is %v, want ErrSetupCancelled", err)
	}
}

// Ctrl+C during the apply stops it: the planner keeps waiting for the goroutine, the context is
// cancelled so Install rolls back, and the run ends as a failure, never as "nothing changed".
func TestSetupCtrlCDuringApplyStopsHonestly(t *testing.T) {
	m := testModel("en", 80)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	m.answers = DefaultAnswers(PresetRelay, "en", "203.0.113.10", 443, "/var/lib/echofog")
	started := make(chan struct{})
	ctxErr := make(chan error, 1)
	m.deps.Apply = func(ctx context.Context, _ *SetupPlan, _ func(string)) (*FinalInfo, error) {
		close(started)
		<-ctx.Done()
		ctxErr <- ctx.Err()
		return nil, ErrInstallStopped
	}
	if _, cmd := m.beginApply(); cmd == nil {
		t.Fatal("the apply goroutine was not started")
	}
	<-started
	m.Update(key(tea.KeyCtrlC))
	if m.cancelled {
		t.Fatal("Ctrl+C during the apply was reported as a clean cancellation")
	}
	var done tea.Msg
	select {
	case done = <-m.events:
	case <-time.After(2 * time.Second):
		t.Fatal("the apply goroutine did not stop after Ctrl+C")
	}
	if err := <-ctxErr; err == nil {
		t.Error("the apply context was not cancelled")
	}
	m.Update(done)
	if m.scr != scrFailed || m.err == nil {
		t.Fatalf("screen %v, error %v: the stopped apply must end as a failure", m.scr, m.err)
	}
	if _, err := m.result(); err == nil || errors.Is(err, ErrSetupCancelled) {
		t.Fatalf("the result is %v, want the real failure", err)
	}
}

// Ctrl+C after a successful apply is not a cancellation: the node is installed and running.
func TestSetupCtrlCAfterSuccessIsNotCancelled(t *testing.T) {
	m := testModel("en", 80)
	m.answers = DefaultAnswers(PresetRelay, "en", "203.0.113.10", 443, "/var/lib/echofog")
	m.scr = scrDone
	m.applied = true
	m.final = &FinalInfo{Phrase: "one two three"}
	m.Update(key(tea.KeyCtrlC))
	if m.cancelled {
		t.Fatal("Ctrl+C after the success was reported as a cancellation")
	}
	res, err := m.result()
	if err != nil {
		t.Fatalf("the result is %v", err)
	}
	if !res.Applied || res.Info == nil {
		t.Fatalf("the result lost the applied plan: %+v", res)
	}
}

// Ctrl+C on the failure screen keeps the failure: the exit code must stay non-zero.
func TestSetupCtrlCOnFailureKeepsError(t *testing.T) {
	m := testModel("en", 80)
	m.scr = scrFailed
	m.err = ErrInstallStopped
	m.Update(key(tea.KeyCtrlC))
	if m.cancelled {
		t.Fatal("Ctrl+C on the failure screen was reported as a cancellation")
	}
	if _, err := m.result(); !errors.Is(err, ErrInstallStopped) {
		t.Fatalf("the result is %v, want the install failure", err)
	}
}

// The phrase file is removed by the path the installer actually wrote, not by the wizard answer.
func TestSetupPhraseFileRemovedByActualPath(t *testing.T) {
	answersDir := t.TempDir()
	finalDir := t.TempDir()
	phraseFile := filepath.Join(finalDir, "recovery-phrase.txt")
	if err := os.WriteFile(phraseFile, []byte("secret words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testModel("en", 80)
	m.answers.DataDir = answersDir
	m.final = &FinalInfo{Phrase: "one two three", DataDir: finalDir}
	m.scr = scrPhrase
	for _, r := range "yes" {
		m.Update(runeKey(string(r)))
	}
	m.Update(key(tea.KeyEnter))
	if m.scr != scrToken {
		t.Fatalf("screen %v, want the token screen", m.scr)
	}
	if _, err := os.Stat(phraseFile); !os.IsNotExist(err) {
		t.Fatal("the phrase file at the installed path was not removed")
	}
}

// The token screen names the file; a format verb must never reach the operator.
func TestSetupTokenScreenHasFileName(t *testing.T) {
	m := testModel("en", 80)
	m.scr = scrToken
	m.final = &FinalInfo{Token: "0123456789abcdef"}
	out := m.View()
	if strings.Contains(out, "%s") {
		t.Errorf("the token screen prints a format verb:\n%s", out)
	}
	if !strings.Contains(out, tokenFileName) {
		t.Errorf("the token screen does not name %s:\n%s", tokenFileName, out)
	}
}
