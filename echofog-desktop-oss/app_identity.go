package main

import (
	"context"
	"errors"
	"time"

	api "github.com/echofog/echofog-core/pkg/api/node"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// panicQuitDelay lets the UI drop its state before the window goes.
const panicQuitDelay = 500 * time.Millisecond

// quitApp closes the app; a package variable so tests can stand in for it.
var quitApp = func(ctx context.Context) {
	if ctx == nil || ctx.Value("events") == nil {
		return // headless or a test: no window to close
	}
	runtime.Quit(ctx)
}

// TriggerPanicButton wipes the long-term keys (identity, wallet, database: memguard enclaves),
// stops the node and closes the app. Closing is what makes the rest go: session keys of chats and
// the messages already decrypted live in this process and in the window, and only the end of the
// process takes them; Go cannot zero what its garbage collector moved.
func (a *App) TriggerPanicButton() error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	wipeErr := nodeAPI.TriggerPanicButton()
	nodeAPI.StopNode()
	a.mu.Lock()
	a.api = nil
	ctx := a.ctx
	a.mu.Unlock()
	safeEventsEmit(ctx, "panic_mode", map[string]any{"at": time.Now().Unix()})
	go func() {
		time.Sleep(panicQuitDelay)
		quitApp(ctx)
	}()
	return wipeErr
}

func (a *App) GetMyPubKey() string {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return ""
	}
	return nodeAPI.GetMyPubKey()
}

// CheckRecoveryPhrase says what is wrong with a typed recovery phrase ("" when it is fine).
func (a *App) CheckRecoveryPhrase(phrase string) string {
	return api.CheckRecoveryPhrase(phrase)
}
