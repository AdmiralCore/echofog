package node

import "errors"

// V2Master returns copies of the SLIP-0010 master key and chain code of the unlocked identity
// (T11.7). The v2 runtime derives its account from them, so it has the same account key as v1
// (AD7) without the recovery phrase ever being kept. The caller wipes both slices.
func (n *Node) V2Master() (key, chain []byte, err error) {
	return nil, nil, errors.New("node: V2Master is not built")
}
