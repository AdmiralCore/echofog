package node

import "errors"

func (n *Node) SignIdentity(data []byte) ([]byte, error) {
	if n.identity == nil {
		return nil, errors.New("identity not loaded")
	}
	return n.identity.SignData(data)
}

func (n *Node) IdentityPublicKey() ([]byte, error) {
	if n.identity == nil {
		return nil, errors.New("identity not loaded")
	}
	return n.identity.GetPublicKey()
}
