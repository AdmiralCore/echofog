package node

// registerModules is the single place a runtime wires the service modules it was built with. It
// calls one file per service so that service branches (dht, chat, mailbox, …) add their own file
// and never conflict with each other when they merge. Each function is idempotent and may be a
// no-op in a build that does not ship the service yet.
func (r *Runtime) registerModules() {
	r.registerDHT()
	r.registerPEX()
	r.registerChat()
	r.registerMailbox()
	r.registerRelay()
	r.registerExit()
	r.registerFiles()
	r.registerCall()
	r.registerAccess()
}
