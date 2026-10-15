package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProfileService_ValidateProfileName(t *testing.T) {
	svc := NewProfileService()

	tests := []struct {
		name        string
		profileName string
		wantErr     bool
	}{
		{"Valid standard name", "default", false},
		{"Valid alphanumeric with dash", "user-vault-01", false},
		{"Valid with dot and underscore", "main_profile.v2", false},
		{"Empty name", "", true},
		{"Whitespace only", "   ", true},
		{"Path traversal attempt slash", "../etc/passwd", true},
		{"Path traversal backslash", "..\\windows\\system32", true},
		{"Spaces in name", "my cool profile", true},
		{"Special symbols", "profile$#@!", true},
		{"Parent directory", "..", true},
		{"Current directory", ".", true},
		{"All dots", "...", true},
		{"Leading dot hidden", ".echofog", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := svc.ValidateProfileName(tt.profileName)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateProfileName(%q) error = %v, wantErr %v", tt.profileName, err, tt.wantErr)
			}
		})
	}
}

func TestProfileService_GetProfileDir(t *testing.T) {
	svc := NewProfileService()

	dir, err := svc.GetProfileDir("test-node")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".echofog", "profiles", "test-node")
	if dir != expected {
		t.Errorf("expected %q, got %q", expected, dir)
	}
}

func TestProfileService_GetProfileDir_RejectsEscape(t *testing.T) {
	svc := &defaultProfileService{home: func() (string, error) { return "/home/u", nil }}
	for _, name := range []string{"..", ".", "...", ".hidden"} {
		if _, err := svc.GetProfileDir(name); err == nil {
			t.Errorf("GetProfileDir(%q) should have been rejected", name)
		}
	}
	dir, err := svc.GetProfileDir("good")
	if err != nil {
		t.Fatalf("GetProfileDir(good): %v", err)
	}
	if want := filepath.Join("/home/u", ".echofog", "profiles", "good"); dir != want {
		t.Errorf("GetProfileDir(good) = %q, want %q", dir, want)
	}
}

func TestProfileService_DeleteProfile_NonExistent(t *testing.T) {
	svc := NewProfileService()

	err := svc.DeleteProfile("definitely-non-existent-profile-xyz987", "pw")
	if err == nil {
		t.Error("expected error when deleting non-existent profile, got nil")
	}
}

// DeleteProfile must refuse "." and ".." before touching the disk: joined onto profiles/ they
// resolve to the profiles directory itself or its parent (~/.echofog), which RemoveAll would wipe.
func TestProfileService_DeleteProfile_RejectsDotNames(t *testing.T) {
	tmp := t.TempDir()
	// Lay down a real profiles directory and a sibling so we can prove nothing is removed.
	profiles := filepath.Join(tmp, ".echofog", "profiles")
	if err := os.MkdirAll(filepath.Join(profiles, "keepme"), 0o700); err != nil {
		t.Fatal(err)
	}
	verifyCalled := false
	svc := &defaultProfileService{
		home:   func() (string, error) { return tmp, nil },
		verify: func(string, string) error { verifyCalled = true; return nil },
	}
	for _, name := range []string{"..", ".", "..."} {
		if err := svc.DeleteProfile(name, "pw"); err == nil {
			t.Errorf("DeleteProfile(%q) should have been rejected", name)
		}
	}
	if verifyCalled {
		t.Error("password verification (and thus any deletion path) should not be reached for dot names")
	}
	if _, err := os.Stat(filepath.Join(profiles, "keepme")); err != nil {
		t.Errorf("a dot-name deletion removed real data: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".echofog")); err != nil {
		t.Errorf("a dot-name deletion removed ~/.echofog: %v", err)
	}
}

// DeleteProfile must require the correct app password: an empty or wrong password leaves the
// profile on disk, so a WebView call alone cannot wipe the user's keys and data.
func TestProfileService_DeleteProfile_RequiresPassword(t *testing.T) {
	tmp := t.TempDir()
	profileDir := filepath.Join(tmp, ".echofog", "profiles", "victim")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wrongPassword := errors.New("wrong password")
	svc := &defaultProfileService{
		home: func() (string, error) { return tmp, nil },
		verify: func(_, pw string) error {
			if pw == "correct" {
				return nil
			}
			return wrongPassword
		},
	}

	if err := svc.DeleteProfile("victim", ""); err == nil {
		t.Error("empty password should be rejected")
	}
	if _, err := os.Stat(profileDir); err != nil {
		t.Fatalf("profile removed despite empty password: %v", err)
	}

	if err := svc.DeleteProfile("victim", "nope"); err == nil {
		t.Error("wrong password should be rejected")
	}
	if _, err := os.Stat(profileDir); err != nil {
		t.Fatalf("profile removed despite wrong password: %v", err)
	}

	if err := svc.DeleteProfile("victim", "correct"); err != nil {
		t.Fatalf("correct password should allow deletion: %v", err)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Error("profile should be gone after a correct-password deletion")
	}
}

func TestProfileService_IsRegistered_Default(t *testing.T) {
	svc := NewProfileService()
	ctx := context.Background()

	// An empty non-existent profile shouldn't panic and should return false
	isReg := svc.IsRegistered(ctx, "nonexistent-test-profile-12345")
	if isReg {
		t.Error("expected non-existent profile to not be registered")
	}
}
