package main

import (
	"context"
	"testing"
)

// After a password change the old password no longer opens the profile, the new one does, and the
// profile remembered on this computer still opens at start.
func TestChangePasswordOfTheApp(t *testing.T) {
	rememberedHome(t, "home", true)

	app := NewApp()
	app.ctx = context.Background()
	if got := app.AutoUnlock(); !got.Unlocked {
		t.Fatalf("start of the app: %+v", got)
	}
	if err := app.ChangePassword("typo", "new-pass-1"); err == nil {
		t.Fatal("a wrong current password must not change it")
	}
	if err := app.ChangePassword(unlockPass, "short"); err == nil {
		t.Fatal("a new password under 8 characters must be refused")
	}
	if err := app.ChangePassword(unlockPass, "new-pass-1"); err != nil {
		t.Fatal(err)
	}
	if err := app.AuthorizePayment("new-pass-1"); err != nil {
		t.Fatalf("payments take the new password at once: %v", err)
	}
	app.Logout()

	app = NewApp()
	app.ctx = context.Background()
	if got := app.AutoUnlock(); !got.Unlocked {
		t.Fatalf("the remembered profile must still open at start: %+v", got)
	}
	app.Lock()
	if err := app.Login(unlockPass); err == nil {
		t.Fatal("the old password still opens the profile")
	}
	if err := app.Login("new-pass-1"); err != nil {
		t.Fatal(err)
	}
	app.Logout()
}
