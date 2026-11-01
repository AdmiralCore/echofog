package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "github.com/echofog/echofog-core/pkg/api/node"
)

const unlockPass = "pass1234"

// rememberedHome makes a profile in a temporary home, remembers its unlock and signs out, as a
// user who switched the option on and closed the app.
func rememberedHome(t *testing.T, profile string, askPayments bool) string {
	t.Helper()
	if !api.RememberSupported() {
		t.Skip("this system cannot remember the unlock yet")
	}
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	app := NewApp()
	app.ctx = context.Background()
	app.SelectProfile(profile)
	if _, err := app.Register(unlockPass, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.RememberUnlock("typo", askPayments); err == nil {
		t.Fatal("a wrong password must not remember the unlock")
	}
	if err := app.RememberUnlock(unlockPass, askPayments); err != nil {
		t.Fatal(err)
	}
	app.Logout()
	return home
}

func rememberFile(home, profile string) string {
	return filepath.Join(home, ".echofog", "profiles", profile, "remembered-unlock")
}

// The next start opens the profile without the password, once: after Lock the password screen
// stays, and the password still opens it.
func TestRememberedProfileOpensAtStart(t *testing.T) {
	rememberedHome(t, "home", false)

	app := NewApp()
	app.ctx = context.Background()
	app.SelectProfile("home")
	if st := app.GetRememberedUnlock(); !st.Supported || !st.Enabled {
		t.Fatalf("before login the option shows %+v", st)
	}
	app.SelectProfile("")

	got := app.AutoUnlock()
	if !got.Unlocked || got.Profile != "home" || got.Reason != "" {
		t.Fatalf("start of the app: %+v", got)
	}
	if err := app.StartNode(); err != nil {
		t.Fatal(err)
	}
	if app.GetMyID() == "" {
		t.Fatal("the node runs without an identity")
	}

	app.Lock()
	if app.getAPI() != nil {
		t.Fatal("Lock must stop the node")
	}
	if app.getActiveProfile() != "home" {
		t.Fatal("Lock keeps the profile for the password screen")
	}
	if again := app.AutoUnlock(); again.Unlocked || again.Profile != "" {
		t.Fatalf("after Lock the app must not open by itself: %+v", again)
	}
	if st := app.GetRememberedUnlock(); !st.Enabled {
		t.Fatal("Lock must keep the remembered unlock")
	}
	if err := app.Login(unlockPass); err != nil {
		t.Fatal(err)
	}
	app.Logout()
}

// A remembered unlock that does not open (another Windows user, a copied profile, damage) gives
// the password screen with a reason, and is dropped.
func TestBadRememberedUnlockAsksThePassword(t *testing.T) {
	home := rememberedHome(t, "home", true)
	if err := os.WriteFile(rememberFile(home, "home"), []byte("not sealed for this user"), 0o600); err != nil {
		t.Fatal(err)
	}

	app := NewApp()
	app.ctx = context.Background()
	got := app.AutoUnlock()
	if got.Unlocked || got.Profile != "home" || got.Reason != "foreign" {
		t.Fatalf("start of the app: %+v", got)
	}
	if _, err := os.Stat(rememberFile(home, "home")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the remembered unlock that does not open must be dropped")
	}
	if app.getActiveProfile() != "home" {
		t.Fatal("the password screen is for the remembered profile")
	}
	if err := app.Login(unlockPass); err != nil {
		t.Fatal(err)
	}
	if app.GetRememberedUnlock().PaymentGate {
		t.Fatal("with nothing remembered, payments go as before")
	}
	app.Logout()
}

// In a session opened without the password, payments ask it, once per payment; switching the
// option off does not lift that, only the password does.
func TestPaymentsAskThePasswordAfterAutoUnlock(t *testing.T) {
	home := rememberedHome(t, "home", true)

	app := NewApp()
	app.ctx = context.Background()
	if got := app.AutoUnlock(); !got.Unlocked {
		t.Fatalf("start of the app: %+v", got)
	}
	defer app.Logout()
	if !app.GetRememberedUnlock().PaymentGate {
		t.Fatal("payments must ask the password")
	}

	if err := app.confirmSendAboveThreshold("TON", "0.1", "addr"); !errors.Is(err, errPaymentPassword) {
		t.Fatalf("a payment without the password: %v", err)
	}
	if err := app.AuthorizePayment("typo"); err == nil {
		t.Fatal("a wrong password must not allow a payment")
	}
	if err := app.confirmSendAboveThreshold("TON", "0.1", "addr"); !errors.Is(err, errPaymentPassword) {
		t.Fatalf("a wrong password allowed a payment: %v", err)
	}
	if err := app.AuthorizePayment(unlockPass); err != nil {
		t.Fatal(err)
	}
	if err := app.confirmSendAboveThreshold("TON", "0.1", "addr"); err != nil {
		t.Fatalf("the payment after the password: %v", err)
	}
	if err := app.confirmSendAboveThreshold("TON", "0.1", "addr"); !errors.Is(err, errPaymentPassword) {
		t.Fatal("the password allows one payment, not the next")
	}

	// a consent older than its minute has gone
	if err := app.AuthorizePayment(unlockPass); err != nil {
		t.Fatal(err)
	}
	app.unlock.mu.Lock()
	app.unlock.payOK = time.Now().Add(-time.Second)
	app.unlock.mu.Unlock()
	if err := app.confirmSendAboveThreshold("TON", "0.1", "addr"); !errors.Is(err, errPaymentPassword) {
		t.Fatal("a stale consent allowed a payment")
	}

	// switching the option off, or deleting its file, leaves this session asking
	if err := app.ForgetUnlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rememberFile(home, "home")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("switching the option off must delete the remembered unlock")
	}
	if err := app.confirmSendAboveThreshold("TON", "0.1", "addr"); !errors.Is(err, errPaymentPassword) {
		t.Fatal("forgetting the unlock lifted the password for payments in a session opened without it")
	}

	// the password lifts it: remembering again with the setting off
	if err := app.RememberUnlock(unlockPass, false); err != nil {
		t.Fatal(err)
	}
	if err := app.confirmSendAboveThreshold("TON", "0.1", "addr"); err != nil {
		t.Fatalf("payments with the setting off: %v", err)
	}
}

// A session opened with the password asks it for payments too while the setting is on, and stops
// when the option is switched off.
func TestPaymentsAskThePasswordAfterPasswordLogin(t *testing.T) {
	rememberedHome(t, "home", true)

	app := NewApp()
	app.ctx = context.Background()
	app.SelectProfile("home")
	if err := app.Login(unlockPass); err != nil {
		t.Fatal(err)
	}
	defer app.Logout()
	if err := app.confirmSendAboveThreshold("TON", "0.1", "addr"); !errors.Is(err, errPaymentPassword) {
		t.Fatalf("a payment without the password: %v", err)
	}
	if err := app.ForgetUnlock(); err != nil {
		t.Fatal(err)
	}
	if err := app.confirmSendAboveThreshold("TON", "0.1", "addr"); err != nil {
		t.Fatalf("with the option off a payment goes as before: %v", err)
	}
}

// The app opens one profile at start: remembering a second forgets the first.
func TestOneRememberedProfile(t *testing.T) {
	home := rememberedHome(t, "first", false)

	app := NewApp()
	app.ctx = context.Background()
	app.SelectProfile("second")
	if _, err := app.Register(unlockPass, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.RememberUnlock(unlockPass, true); err != nil {
		t.Fatal(err)
	}
	app.Logout()
	if _, err := os.Stat(rememberFile(home, "first")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the first profile must be forgotten")
	}

	app = NewApp()
	app.ctx = context.Background()
	if got := app.AutoUnlock(); !got.Unlocked || got.Profile != "second" {
		t.Fatalf("start of the app: %+v", got)
	}
	app.Logout()
}
