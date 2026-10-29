package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Wallet send methods are bound to the WebView, so a future XSS in the UI could call them directly
// and move funds. Above these amounts a send must be confirmed in a native OS dialog, which the web
// layer cannot script, suppress or fake. No XSS exists today (the frontend uses no {@html} or
// innerHTML); this is defence in depth. Amounts at or below the threshold go through unprompted so
// small, frequent payments (per-chunk traffic payments) are not interrupted.
const (
	confirmThresholdTON  = 1.0
	confirmThresholdUSDT = 1.0
)

// dialogFunc is the native dialog; a package variable so tests can stand in for it.
var dialogFunc = runtime.MessageDialog

func thresholdFor(asset string) float64 {
	if strings.EqualFold(strings.TrimSpace(asset), "usdt") {
		return confirmThresholdUSDT
	}
	return confirmThresholdTON
}

// aboveThreshold reports whether a decimal amount string meets or exceeds threshold. An amount that
// does not parse is treated as above it, so a malformed value is confirmed rather than waved through.
func aboveThreshold(amount string, threshold float64) bool {
	v, err := strconv.ParseFloat(strings.TrimSpace(amount), 64)
	if err != nil {
		return true
	}
	return v >= threshold
}

// confirmNative shows a native yes/no dialog and returns nil only if the user approves. In headless
// mode (no Wails frontend, so no WebView to attack) there is nothing to confirm against and it
// allows the action.
func (a *App) confirmNative(title, message string) error {
	a.mu.RLock()
	ctx := a.ctx
	a.mu.RUnlock()
	if ctx == nil || ctx.Value("events") == nil {
		return nil
	}
	res, err := dialogFunc(ctx, runtime.MessageDialogOptions{
		Type:          runtime.QuestionDialog,
		Title:         title,
		Message:       message,
		Buttons:       []string{"Yes", "No"},
		DefaultButton: "No",
		CancelButton:  "No",
	})
	if err != nil {
		return fmt.Errorf("could not confirm the action: %w", err)
	}
	// Windows returns the native button label ("Yes"/"No"); other platforms return the custom label.
	if !strings.EqualFold(res, "yes") {
		return errors.New("cancelled")
	}
	return nil
}

// confirmSendAboveThreshold prompts for confirmation when amount of asset meets the asset's
// threshold. Below it, it is a no-op.
func (a *App) confirmSendAboveThreshold(asset, amount, to string) error {
	key := strings.ToUpper(strings.TrimSpace(asset))
	if !aboveThreshold(amount, thresholdFor(asset)) {
		// a small send goes through unprompted, but small sends add up: many of them would drain
		// the wallet one at a time, so once the sends left unconfirmed within the hour reach the
		// threshold, this one is confirmed too
		v, _ := strconv.ParseFloat(strings.TrimSpace(amount), 64)
		if quietSends.add(key, v, time.Now()) < thresholdFor(asset) {
			return nil
		}
	}
	if err := a.confirmNative("Confirm transfer", fmt.Sprintf("Send %s %s to\n%s?", amount, key, to)); err != nil {
		return err
	}
	quietSends.reset(key)
	return nil
}

// quietSends adds up the sends that went through without a prompt, per asset, over an hour.
var quietSends = &quietLedger{sends: map[string][]quietSend{}}

const quietWindow = time.Hour

type quietSend struct {
	at     time.Time
	amount float64
}

type quietLedger struct {
	mu    sync.Mutex
	sends map[string][]quietSend
}

// add records amount for asset and returns the total sent unprompted within the window,
// this one included.
func (l *quietLedger) add(asset string, amount float64, now time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.sends[asset][:0]
	total := amount
	for _, s := range l.sends[asset] {
		if now.Sub(s.at) < quietWindow {
			kept = append(kept, s)
			total += s.amount
		}
	}
	l.sends[asset] = append(kept, quietSend{now, amount})
	return total
}

func (l *quietLedger) reset(asset string) {
	l.mu.Lock()
	delete(l.sends, asset)
	l.mu.Unlock()
}
