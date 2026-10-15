package node

// PanicWipe wipes the identity's key material from memory at once, so a device seized while the app
// is unlocked yields no keys from RAM. It does not change anything on disk: the encrypted identity
// remains and the same app password unlocks it again, so the node is simply locked until the user
// logs in. It is a lock, not a self-destruct; deleting the profile is what erases the data.
func (n *Node) PanicWipe() error {
	if n.identity != nil {
		n.identity.ActivatePanicButton()
	}
	return nil
}
