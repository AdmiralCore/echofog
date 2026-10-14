package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// After the password the screen loads chats, groups, the network, the proxy and the wallet: none
// of that may bring the app down, also for a profile written by an older version.
func TestLoginFlowOfTheApp(t *testing.T) {
	src := os.Getenv("OLD_PROFILE_DIR") // a profile db made by an older version, optional
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	app := NewApp()
	app.ctx = context.Background()
	if src != "" {
		dst := filepath.Join(home, ".echofog", "profiles", "old", "db")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("cp", "-r", src, dst).CombinedOutput(); err != nil {
			t.Fatalf("copy: %v %s", err, out)
		}
		app.SelectProfile("old")
	} else {
		app.SelectProfile("fresh")
		if _, err := app.Register("pass123", ""); err != nil {
			t.Fatal(err)
		}
		if err := app.StartNode(); err != nil {
			t.Fatal(err)
		}
		app.Logout()
		app.SelectProfile("fresh")
	}

	if err := app.Login("pass123"); err != nil {
		t.Fatal(err)
	}
	if err := app.StartNode(); err != nil {
		t.Fatal(err)
	}
	defer app.Logout()

	_ = app.GetMyID()
	_ = app.GetMyPubKey()
	contacts, _ := app.GetContacts()
	for _, c := range contacts {
		_, _ = app.GetChatHistory(c.PeerID, 200, 0)
	}
	groups, _ := app.ListGroups()
	for _, g := range groups {
		_, _ = app.GetGroupMessages(g.ID, 200)
		_, _ = app.ListGroupMembers(g.ID)
		_, _ = app.GetGroupInvite(g.ID)
	}
	_ = app.GetNetworkStatus()
	_, _ = app.GetSettings()
	_ = app.GetVPNStatus()
	_ = app.GetExitPayment()
	_ = app.GetSharingStatus()
	_ = app.ListExitNodes()
	_ = app.GetProbeDomains()
	_, _ = app.GetTonWallet()
	_, _ = app.GetTonBalance()
	_, _ = app.GetTonHistory(20)
	_ = app.GetSupport()
	t.Logf("contacts %d, groups %d", len(contacts), len(groups))
}

// Restoring from the recovery phrase also asks for a password.
func TestRecoverFlowOfTheApp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	app := NewApp()
	app.ctx = context.Background()
	app.SelectProfile("a")
	phrase, err := app.Register("pass123", "")
	if err != nil {
		t.Fatal(err)
	}
	app.Logout()
	app.SelectProfile("b")
	if err := app.Recover(phrase, "", "newpass1"); err != nil {
		t.Fatal(err)
	}
	if err := app.StartNode(); err != nil {
		t.Fatal(err)
	}
	_, _ = app.GetContacts()
	_, _ = app.GetTonWallet()
	app.Logout()
}
