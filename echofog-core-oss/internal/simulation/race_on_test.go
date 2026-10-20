//go:build race

package simulation

// raceBuild: the race detector slows everything several times, so ten nodes in one process fall
// behind real time and drop media the way an overloaded computer would. Delivery thresholds that
// measure the protocol, not the machine, are relaxed under it.
const raceBuild = true
