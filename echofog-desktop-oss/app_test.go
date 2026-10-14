package main

import (
	"context"
	"io/fs"
	"strings"
	"sync"
	"testing"
)

type mockProfileService struct {
	profiles   []string
	registered bool
}

func (m *mockProfileService) GetProfiles() ([]string, error) {
	return m.profiles, nil
}

func (m *mockProfileService) DeleteProfile(name string) error {
	newProfiles := make([]string, 0)
	for _, p := range m.profiles {
		if p != name {
			newProfiles = append(newProfiles, p)
		}
	}
	m.profiles = newProfiles
	return nil
}

func (m *mockProfileService) ValidateProfileName(name string) error {
	if name == "" {
		return context.Canceled
	}
	return nil
}

func (m *mockProfileService) IsRegistered(ctx context.Context, profileName string) bool {
	return m.registered
}

func (m *mockProfileService) GetProfileDir(profileName string) (string, error) {
	return "/tmp/echofog/" + profileName, nil
}

func TestApp_LifecycleAndDefaults(t *testing.T) {
	app := NewApp()
	mockSvc := &mockProfileService{
		profiles:   []string{"vault1", "vault2"},
		registered: true,
	}
	app.SetProfileService(mockSvc)

	profiles := app.GetProfiles()
	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profiles))
	}

	app.SelectProfile("vault1")
	if app.getActiveProfile() != "vault1" {
		t.Fatalf("expected activeProfile 'vault1', got %s", app.getActiveProfile())
	}

	if !app.IsRegistered() {
		t.Fatal("expected IsRegistered to return true based on mock service")
	}

	// Delete active profile should logout and clear activeProfile
	err := app.DeleteProfile("vault1")
	if err != nil {
		t.Fatalf("unexpected error deleting profile: %v", err)
	}

	if app.getActiveProfile() != "" {
		t.Errorf("expected activeProfile to be cleared, got %q", app.getActiveProfile())
	}
}

func TestApp_ThreadSafety_Concurrency(t *testing.T) {
	app := NewApp()
	mockSvc := &mockProfileService{
		profiles:   []string{"alpha", "beta", "gamma"},
		registered: false,
	}
	app.SetProfileService(mockSvc)

	var wg sync.WaitGroup
	workers := 50

	// Launch concurrent readers and writers
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			if idx%2 == 0 {
				app.SelectProfile("profile-" + string(rune('a'+(idx%26))))
			} else {
				_ = app.getActiveProfile()
				_ = app.GetProfiles()
				_ = app.IsRegistered()
				_ = app.GetMyID()
				_ = app.GetNetworkStatus()
			}
		}(i)
	}

	wg.Wait()
}

func TestApp_UninitializedNode_Errors(t *testing.T) {
	app := NewApp()

	// Calling Login without selecting profile must return error
	err := app.Login("mypassword")
	if err == nil || !strings.Contains(err.Error(), "select a profile") {
		t.Errorf("expected error about selecting profile, got: %v", err)
	}

	// Calling StartNode before Login or Register must return error
	err = app.StartNode()
	if err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("expected error about node not initialized, got: %v", err)
	}

	// Calling SendMessage before node started
	err = app.SendMessage("pubkey", "hello")
	if err == nil || !strings.Contains(err.Error(), "not started") {
		t.Errorf("expected 'node not started' error, got: %v", err)
	}
}

func TestAssetsEmbed(t *testing.T) {
	// Check if fs.Sub works
	sub, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		t.Fatalf("fs.Sub error: %v", err)
	}
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("fs.ReadFile on sub error: %v", err)
	}
	t.Logf("Read from sub index.html: %d bytes", len(data))
}

func TestApp_CallBridge_Uninitialized(t *testing.T) {
	app := NewApp()

	// Calling call methods before node is logged in / started should return errors gracefully
	_, err := app.JoinRoom("room1", "")
	if err == nil {
		t.Error("expected error calling JoinRoom before login")
	}

	err = app.SubmitCallAnswer("sdp")
	if err == nil {
		t.Error("expected error calling SubmitCallAnswer before login")
	}

	err = app.LeaveRoom()
	if err != nil {
		t.Errorf("LeaveRoom when uninitialized should return nil, got %v", err)
	}

	err = app.StartScreenShare("stream1", 3500)
	if err == nil {
		t.Error("expected error calling StartScreenShare before login")
	}

	err = app.StopScreenShare("stream1")
	if err == nil {
		t.Error("expected error calling StopScreenShare before login")
	}

	err = app.WatchStream("stream1")
	if err == nil {
		t.Error("expected error calling WatchStream before login")
	}

	err = app.UnwatchStream("stream1")
	if err == nil {
		t.Error("expected error calling UnwatchStream before login")
	}

	// SetVADState should not panic
	app.SetVADState(true, 42.0)
}
