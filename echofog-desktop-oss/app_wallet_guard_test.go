package main

import (
	"context"
	"errors"
	"testing"

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
