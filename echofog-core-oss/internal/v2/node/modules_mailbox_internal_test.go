package node

import "testing"

func TestProfileHasRole(t *testing.T) {
	roles := []Role{RoleRelay, RoleMailbox}
	if !profileHasRole(roles, RoleMailbox) {
		t.Fatal("mailbox role not found")
	}
	if profileHasRole(roles, RoleExit) || profileHasRole(nil, RoleMailbox) {
		t.Fatal("role reported that the profile does not have")
	}
}
