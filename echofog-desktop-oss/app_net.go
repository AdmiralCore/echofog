package main

import (
	"errors"
	"fmt"
	"time"

	api "github.com/echofog/echofog-core/pkg/api/node"
)

func (a *App) GetVPNStatus() api.VPNStatus {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.VPNStatus{TunnelNotice: "node not started"}
	}
	return nodeAPI.GetVPNStatus()
}

func (a *App) StartVPN(relayPeerID string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.StartVPN(relayPeerID)
}

func (a *App) StopVPN() error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil
	}
	return nodeAPI.StopVPN()
}

// SetWholeComputer routes every application through the exit (asks for administrator rights).
func (a *App) SetWholeComputer(on bool) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.SetWholeComputer(on)
}

// SetVPNAutoSwitch: the proxy moves to another working exit by itself when this one stops working.
func (a *App) SetVPNAutoSwitch(on bool) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.SetVPNAutoSwitch(on)
}

// GetExitPayment describes what the connected exit costs and what is left of what was paid.
func (a *App) GetExitPayment() api.ExitPaymentStatus {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.ExitPaymentStatus{}
	}
	return nodeAPI.GetExitPayment()
}

// SetAutoPay switches automatic top-ups on or off (amounts in USDT).
func (a *App) SetAutoPay(enabled bool, chunkUSDT, dailyCapUSDT string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	// Enabling auto-pay authorises recurring spending without a further prompt, so confirm it
	// natively; turning it off needs no confirmation.
	if enabled {
		if err := a.confirmNative("Confirm automatic payments",
			fmt.Sprintf("Allow EchoFog to pay for traffic automatically, up to %s USDT per day?", dailyCapUSDT)); err != nil {
			return err
		}
	}
	return nodeAPI.SetAutoPay(enabled, chunkUSDT, dailyCapUSDT)
}

// SetPayAsset chooses how traffic is paid: "auto", "TON" or "USDT".
func (a *App) SetPayAsset(asset string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.SetPayAsset(asset)
}

// PayExit pays the connected exit for traffic now.
func (a *App) PayExit(amountUSDT string) (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errors.New("node not started")
	}
	// a payment the page asks for, like any other send: confirmed natively above the threshold
	if err := a.confirmSendAboveThreshold("USDT", amountUSDT, "the exit you are connected to"); err != nil {
		return "", err
	}
	ctx, cancel := a.tonContext(90 * time.Second)
	defer cancel()
	return nodeAPI.PayExit(ctx, amountUSDT)
}

// GetVPNHistory lists the exits used, newest first, with traffic and how each one served.
func (a *App) GetVPNHistory() []api.VPNSession {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil
	}
	return nodeAPI.GetVPNHistory()
}

// ClearVPNHistory forgets the exits used.
func (a *App) ClearVPNHistory() error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.ClearVPNHistory()
}

// ConnectLastExit connects through the exit used most recently.
func (a *App) ConnectLastExit() error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.ConnectLastExit()
}

// GetSavedExits lists the exits used or starred, one entry each, favourites first.
func (a *App) GetSavedExits() []api.SavedExit {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil
	}
	return nodeAPI.GetSavedExits()
}

// SetExitName names an exit ("" removes the name).
func (a *App) SetExitName(peerID, name string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.SetExitName(peerID, name)
}

// SetExitFavorite stars an exit or takes the star away.
func (a *App) SetExitFavorite(peerID string, on bool) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.SetExitFavorite(peerID, on)
}

// ForgetExit removes an exit from the history, with its name and star.
func (a *App) ForgetExit(peerID string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.ForgetExit(peerID)
}

// StopVPNForSwitch stops the proxy before connecting through another exit; with the kill switch
// on, whole-computer mode stays up and blocks the gap instead of letting apps out directly.
func (a *App) StopVPNForSwitch() error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil
	}
	return nodeAPI.StopVPNForSwitch()
}

// SetKillSwitch turns the kill switch of whole-computer mode on or off.
func (a *App) SetKillSwitch(on bool) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.SetKillSwitch(on)
}

// KillSwitchHeld reports whether the kill switch holds the computer's traffic after EchoFog went
// away without turning whole-computer mode off. It works before login.
func (a *App) KillSwitchHeld() bool { return api.KillSwitchHeld() }

// ReleaseKillSwitch lets the computer back on its own network after a held kill switch.
func (a *App) ReleaseKillSwitch() error { return api.ReleaseKillSwitch() }
