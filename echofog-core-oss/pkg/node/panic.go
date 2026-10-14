package node

// PanicWipe wipes the identity's key material from memory at once. After it, nothing stored on the
// device can be decrypted without the password (or the recovery phrase), and the node is unusable
// until the user logs in again.
func (n *Node) PanicWipe() error {
	if n.identity != nil {
		n.identity.ActivatePanicButton()
	}
	return nil
}
