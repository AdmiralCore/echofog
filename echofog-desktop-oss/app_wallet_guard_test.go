package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

func TestAboveThreshold(t *testing.T) {
	cases := []struct {
		amount string
		thr    float64
		want   bool
	}{
		{"0.5", 1.0, false},
		{"0.999", 1.0, false},
		{"1", 1.0, true},
		{"2.5", 1.0, true},
		{"", 1.0, true},    // unparseable -> confirm to be safe
		{"abc", 1.0, true}, // unparseable -> confirm to be safe
		{"0.01", 1.0, false},
	}
	for _, c := range cases {
		if got := aboveThreshold(c.amount, c.thr); got != c.want {
			t.Errorf("aboveThreshold(%q, %v) = %v, want %v", c.amount, c.thr, got, c.want)
		}
	}
}

// withEventsCtx returns an App whose context looks like a live Wails frontend, so confirmNative
// reaches the (stubbed) dialog instead of taking the headless no-op path.
func withEventsCtx() *App {
	app := NewApp()
	app.ctx = context.WithValue(context.Background(), "events", struct{}{})
	return app
}

func TestConfirmSendAboveThreshold(t *testing.T) {
	orig := dialogFunc
	defer func() { dialogFunc = orig }()

	app := withEventsCtx()

	// Below threshold: never prompts, always allowed.
	prompted := false
	dialogFunc = func(ctx context.Context, o runtime.MessageDialogOptions) (string, error) {
		prompted = true
		return "No", nil
	}
	if err := app.confirmSendAboveThreshold("TON", "0.5", "addr"); err != nil {
		t.Errorf("small send should pass without confirmation: %v", err)
	}
	if prompted {
		t.Error("a below-threshold send must not prompt")
	}

	// Above threshold, user declines -> refused.
	prompted = false
	dialogFunc = func(ctx context.Context, o runtime.MessageDialogOptions) (string, error) {
		prompted = true
		return "No", nil
	}
	if err := app.confirmSendAboveThreshold("TON", "5", "addr"); err == nil {
		t.Error("a declined large send must be refused")
	}
	if !prompted {
		t.Error("an above-threshold send must prompt")
	}

	// Above threshold, user approves -> allowed.
	dialogFunc = func(ctx context.Context, o runtime.MessageDialogOptions) (string, error) {
		return "Yes", nil
	}
	if err := app.confirmSendAboveThreshold("TON", "5", "addr"); err != nil {
		t.Errorf("an approved large send should pass: %v", err)
	}

	// A dialog error is treated as not-confirmed (fail closed).
	dialogFunc = func(ctx context.Context, o runtime.MessageDialogOptions) (string, error) {
		return "", errors.New("no display")
	}
	if err := app.confirmSendAboveThreshold("TON", "5", "addr"); err == nil {
		t.Error("a dialog error must refuse the send")
	}
}

// Headless (no live frontend context) must not block sends: there is no WebView to attack there.
func TestConfirmNative_HeadlessAllows(t *testing.T) {
	orig := dialogFunc
	defer func() { dialogFunc = orig }()
	dialogFunc = func(ctx context.Context, o runtime.MessageDialogOptions) (string, error) {
		t.Fatal("dialog must not be shown in headless mode")
		return "", nil
	}
	app := NewApp() // ctx is nil
	if err := app.confirmNative("t", "m"); err != nil {
		t.Errorf("headless confirm should allow: %v", err)
	}
}

// Small sends add up: once those left unconfirmed within the hour reach the threshold, the next
// one asks, so a page cannot drain the wallet in many sends just under it. After a yes the count
// starts again; each asset counts on its own.
func TestSmallSendsAddUpToAConfirmation(t *testing.T) {
	orig := dialogFunc
	defer func() { dialogFunc = orig }()
	quietSends.reset("TON")
	quietSends.reset("USDT")
	app := withEventsCtx()

	prompts, answer := 0, "No"
	dialogFunc = func(ctx context.Context, o runtime.MessageDialogOptions) (string, error) {
		prompts++
		return answer, nil
	}
	for i := 0; i < 3; i++ {
		if err := app.confirmSendAboveThreshold("TON", "0.3", "addr"); err != nil {
			t.Fatalf("send %d of 0.3 TON should pass unprompted: %v", i+1, err)
		}
	}
	if prompts != 0 {
		t.Fatalf("0.9 TON in small sends prompted %d times", prompts)
	}
	if err := app.confirmSendAboveThreshold("TON", "0.3", "addr"); err == nil {
		t.Fatal("the send that takes the unconfirmed total to 1.2 TON must ask, and a no refuses it")
	}
	if prompts != 1 {
		t.Fatalf("want one prompt, got %d", prompts)
	}
	if err := app.confirmSendAboveThreshold("USDT", "0.5", "addr"); err != nil {
		t.Errorf("USDT counts apart from TON: %v", err)
	}

	answer = "Yes"
	if err := app.confirmSendAboveThreshold("TON", "0.3", "addr"); err != nil {
		t.Fatalf("a yes lets it through: %v", err)
	}
	if err := app.confirmSendAboveThreshold("TON", "0.3", "addr"); err != nil {
		t.Errorf("after a yes the count starts again: %v", err)
	}
	if prompts != 2 {
		t.Errorf("want two prompts in all, got %d", prompts)
	}

	// the window: sends older than an hour no longer count
	l := &quietLedger{sends: map[string][]quietSend{}}
	start := time.Now()
	l.add("TON", 0.9, start)
	if got := l.add("TON", 0.3, start.Add(quietWindow+time.Minute)); got != 0.3 {
		t.Errorf("an hour later the old send should not count: total %v", got)
	}
}
