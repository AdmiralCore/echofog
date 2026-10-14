package main

import (
	"errors"
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
	ctx, cancel := a.tonContext(90 * time.Second)
	defer cancel()
	return nodeAPI.PayExit(ctx, amountUSDT)
}
