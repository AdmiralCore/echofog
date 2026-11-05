package node

// registerDHT registers dht/2 (T5.1).
//
// The service package owns the handler and calls r.Register with the built-in spec. Keep this
// file present even while the service is not implemented: it is the merge point for dht/2, so
// the service branch never edits modules.go or another service's file.
func (r *Runtime) registerDHT() {
	// Registered by T5.1 when the service package is merged.
}
