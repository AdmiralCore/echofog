package main

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	api "github.com/echofog/echofog-core/pkg/api/node"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// "Remember on this computer": the profile opens without its password for this Windows user, so
// the node comes back by itself after a restart (people who share an exit or a relay from home
// keep it running). It is the user's choice and off by default. The password stays the way in on
// another computer, for another user, and once the option is off. The key is kept by the core
// (pkg/node/remember.go), sealed with DPAPI; only one profile is remembered at a time, the one the
// app opens at start.
//
// Payments: with "ask the password before payments" (on by default) a session asks the app
// password before any money leaves the wallet, however it was opened. Only the password lifts
// that for the session: a session opened without it keeps asking even if the remembered unlock is
// switched off or its file is deleted while the app runs.

// paymentAuthWindow is how long the password given for a payment lets that payment go out.
const paymentAuthWindow = time.Minute

// errPaymentPassword: payments ask the password in this session and it was not given.
var errPaymentPassword = errors.New("enter the app password to confirm this payment")

// unlockState is the remembered unlock as this run of the app sees it.
type unlockState struct {
	mu        sync.Mutex
	autoTried bool      // the remembered unlock is tried once per start of the app, not after Lock or a sign-out
	byKey     bool      // this session was opened without the password
	payGate   bool      // payments ask the password in this session
	payOK     time.Time // one payment may go out until then (AuthorizePayment)
}

// opened records how a session was opened and whether its payments ask the password.
func (s *unlockState) opened(byKey, payGate bool) {
	s.mu.Lock()
	s.byKey, s.payGate, s.payOK = byKey, payGate, time.Time{}
	s.mu.Unlock()
}

// closed forgets the session (Lock, sign-out); the start-of-app attempt is not made again.
func (s *unlockState) closed() { s.opened(false, false) }

// RememberedUnlockDTO is what the app shows about "remember on this computer".
type RememberedUnlockDTO struct {
	Supported   bool `json:"supported"`   // this system can keep the unlock (only Windows yet)
	Enabled     bool `json:"enabled"`     // the chosen profile opens without its password here
	AskPayments bool `json:"askPayments"` // the setting kept with it (known once logged in)
	PaymentGate bool `json:"paymentGate"` // payments ask the password in this session
}

// GetRememberedUnlock reports the option for the chosen profile, also before login.
func (a *App) GetRememberedUnlock() RememberedUnlockDTO {
	dto := RememberedUnlockDTO{Supported: api.RememberSupported()}
	if nodeAPI := a.getAPI(); nodeAPI != nil {
		dto.Enabled, dto.AskPayments = nodeAPI.RememberedUnlock()
	} else if profile := a.getActiveProfile(); profile != "" {
		dto.Enabled = api.IsRemembered(profile)
	}
	a.unlock.mu.Lock()
	dto.PaymentGate = a.unlock.payGate
	a.unlock.mu.Unlock()
	return dto
}

// RememberUnlock switches the option on for the logged-in profile, or changes whether payments
// ask the password. It takes the app password: a page in the window cannot do it on its own. Any
// other profile remembered before is forgotten: the app opens one profile at start.
func (a *App) RememberUnlock(appPassword string, askPayments bool) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	if err := nodeAPI.RememberUnlock(appPassword, askPayments); err != nil {
		if errors.Is(err, api.ErrWrongPassword) {
			return errors.New("wrong app password")
		}
		return err
	}
	profile := a.getActiveProfile()
	for _, p := range a.GetProfiles() {
		if p != profile && api.IsRemembered(p) {
			if err := api.ForgetUnlock(p); err != nil {
				slog.Warn("Could not forget the unlock of another profile", "profile", p, "err", err)
			}
		}
	}
	a.unlock.mu.Lock()
	a.unlock.payGate = askPayments
	a.unlock.payOK = time.Time{}
	a.unlock.mu.Unlock()
	slog.Info("The unlock is remembered on this computer", "profile", profile, "ask_payments", askPayments)
	return nil
}

// ForgetUnlock switches the option off: the next start asks the password. It needs no password,
// since it only takes a way in away.
func (a *App) ForgetUnlock() error {
	profile := a.getActiveProfile()
	if profile == "" {
		return errors.New("please select a profile first")
	}
	if err := api.ForgetUnlock(profile); err != nil {
		return err
	}
	a.unlock.mu.Lock()
	if !a.unlock.byKey {
		a.unlock.payGate = false
	}
	a.unlock.mu.Unlock()
	slog.Info("The remembered unlock is forgotten", "profile", profile)
	return nil
}

// AutoUnlockDTO is the outcome of the start-of-app attempt.
type AutoUnlockDTO struct {
	Profile  string `json:"profile"`  // the remembered profile; "" when there is none
	Unlocked bool   `json:"unlocked"` // logged in: StartNode comes next
	// Reason says why the password is asked after all: "foreign" (another Windows user or
	// computer), "stale" (sealed again since), "damaged", or "failed" (Error says what; the
	// remembered unlock is kept for the next start).
	Reason string `json:"reason"`
	Error  string `json:"error"`
}

// AutoUnlock logs in to the remembered profile without the password. It runs once per start of
// the app: after Lock or a sign-out the password screen stays.
func (a *App) AutoUnlock() AutoUnlockDTO {
	a.unlock.mu.Lock()
	tried := a.unlock.autoTried
	a.unlock.autoTried = true
	a.unlock.mu.Unlock()
	if tried || !api.RememberSupported() {
		return AutoUnlockDTO{}
	}
	profile := ""
	for _, p := range a.GetProfiles() {
		if api.IsRemembered(p) {
			profile = p
			break
		}
	}
	if profile == "" {
		return AutoUnlockDTO{}
	}

	a.mu.Lock()
	if a.api != nil {
		a.mu.Unlock()
		return AutoUnlockDTO{}
	}
	a.activeProfile = profile
	cfg := api.GetConfiguredConfig(profile)
	node := coreNode.NewNode(a.ctx, cfg, profile)
	nodeAPI := api.NewNodeApi(node)
	askPayments, err := nodeAPI.LoginRemembered()
	if err != nil {
		a.mu.Unlock()
		slog.Warn("The remembered unlock did not open the profile; asking the password", "profile", profile, "error", err)
		out := AutoUnlockDTO{Profile: profile, Reason: "failed", Error: err.Error()}
		switch {
		case errors.Is(err, api.ErrRememberedForeign):
			out.Reason, out.Error = "foreign", ""
		case errors.Is(err, api.ErrRememberedStale):
			out.Reason, out.Error = "stale", ""
		case errors.Is(err, api.ErrRememberedDamaged):
			out.Reason, out.Error = "damaged", ""
		}
		return out
	}
	a.api = nodeAPI
	a.mu.Unlock()

	a.unlock.opened(true, askPayments)
	slog.Info("Opened the profile with the unlock remembered on this computer", "profile", profile)
	return AutoUnlockDTO{Profile: profile, Unlocked: true}
}

// Lock stops the node and goes back to the password screen of the same profile. The remembered
// unlock stays: the next start of the app opens without the password again.
func (a *App) Lock() {
	a.mu.Lock()
	if a.api != nil {
		a.api.StopNode()
		a.api = nil
	}
	a.mu.Unlock()
	a.unlock.closed()
	slog.Info("Locked: the node is stopped until the password")
}

// AuthorizePayment checks the app password and lets the next payment of this session go out
// (within a minute).
func (a *App) AuthorizePayment(appPassword string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errNodeNotStarted
	}
	if err := nodeAPI.VerifyPassword(appPassword); err != nil {
		return errors.New("wrong app password")
	}
	a.unlock.mu.Lock()
	a.unlock.payOK = time.Now().Add(paymentAuthWindow)
	a.unlock.mu.Unlock()
	return nil
}

// checkPaymentPassword lets a payment go out when this session does not ask the password, or the
// password was just given for it; that consent is used up by this payment.
func (a *App) checkPaymentPassword() error {
	a.unlock.mu.Lock()
	defer a.unlock.mu.Unlock()
	if !a.unlock.payGate {
		return nil
	}
	if time.Now().After(a.unlock.payOK) {
		return errPaymentPassword
	}
	a.unlock.payOK = time.Time{}
	return nil
}
