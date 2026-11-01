package node

// ChangePassword replaces the app password of the logged-in profile after checking the current
// one; a remembered unlock follows the new password (see node.ChangePassword).
func (n *NodeApi) ChangePassword(oldPassword, newPassword string) error {
	return n.node.ChangePassword([]byte(oldPassword), []byte(newPassword))
}
