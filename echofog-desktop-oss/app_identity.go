package main

import (
	"errors"
	api "github.com/echofog/echofog-core/pkg/api/node"
	"time"
)

// TriggerPanicButton wipes the decrypted keys from memory, stops the node and tells the UI to drop its state.
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
