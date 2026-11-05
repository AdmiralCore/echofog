package node

// registerChat registers chat/2 (T7.1/T7.2).
//
// The service package owns the handler and calls r.Register with the built-in spec. Keep this
// file present even while the service is not implemented: it is the merge point for chat/2, so
// the service branch never edits modules.go or another service's file.
func (r *Runtime) registerChat() {
	// Registered by T7.1/T7.2 when the service package is merged.
}
