package main

import (
	"context"
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

func TestProfileService_DeleteProfile_NonExistent(t *testing.T) {
	svc := NewProfileService()

	err := svc.DeleteProfile("definitely-non-existent-profile-xyz987")
	if err == nil {
		t.Error("expected error when deleting non-existent profile, got nil")
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
