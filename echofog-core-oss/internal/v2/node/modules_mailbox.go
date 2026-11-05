package node

// registerMailbox registers mailbox/2 (T7.4).
//
// The service package owns the handler and calls r.Register with the built-in spec. Keep this
// file present even while the service is not implemented: it is the merge point for mailbox/2,
// so the service branch never edits modules.go or another service's file.
func (r *Runtime) registerMailbox() {
	// Registered by T7.4 when the service package is merged.
}
