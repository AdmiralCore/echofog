package node

import "errors"

// PublishNamespace is the store namespace of the node's own publication state (AD13): the issued
// device list under the key "devices".
const PublishNamespace = "v2/publish"

// PublishOwn registers an own object for storing in the DHT and for republication: the single
// helper for card/2, devices/2, prekeys/2 and mailbox-hint/2 (T4.2b).
func (r *Runtime) PublishOwn(raw []byte) error {
	return errors.New("node: not implemented")
}
