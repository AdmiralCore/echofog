package node

// OwnRecords returns the records this node asks the DHT client to publish by itself: the device
// list, the public card (when it has one) and the chat prekeys. A host that has no DHT network yet
// (a test of two runtimes in one process) hands them to a DHT server directly.
func (r *Runtime) OwnRecords() [][]byte { return r.ownDHTRecords() }
