// Package circuitswitch holds the end-to-end test of a call's relay circuit: its round trip and
// its move to another relay, between real nodes on this machine. It starts five nodes, so it runs
// in a test process of its own (one process may lock only so much memory for the nodes' keys).
package circuitswitch
