package main

import "errors"

// These are features the original desktop UI referenced that the open-source build does not ship
// (an in-chat crypto-voucher flow and an AI command bar). They are stubbed so the ported UI
// compiles and runs; the actions report that they are unavailable, and peer balance reads as zero.

var errFeatureUnavailable = errors.New("this feature is not available in this build")

// CommandAI is a placeholder for the removed AI command bar.
func (a *App) CommandAI(prompt string) (string, error) {
	return "", errFeatureUnavailable
}

// GetPeerBalance reports the balance a peer owes/holds; the voucher ledger is not in this build.
func (a *App) GetPeerBalance(peerID string) (string, error) {
	return "0", nil
}

// IssueVoucher, AcceptVoucher and ClaimPeerVoucher are the removed in-chat crypto-voucher flow.
func (a *App) IssueVoucher(pubKey string, recipientWallet string, amount float64) error {
	return errFeatureUnavailable
}

func (a *App) AcceptVoucher(peerID string, raw string) error {
	return errFeatureUnavailable
}

func (a *App) ClaimPeerVoucher(peerID string) error {
	return errFeatureUnavailable
}
