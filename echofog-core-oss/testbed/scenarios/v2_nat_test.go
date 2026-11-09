package scenarios

import (
	"testing"
	"time"
)

// v2-nat: the product's punch engine, an initiator and a responder, against a model of four NAT
// types. Direct wherever the NAT physics allows it, the relay where it does not, and never a spray
// of ports (the birthday punch is the owner's and is not part of the product's engine).
func TestNATScenarioIsGreenInTheModel(t *testing.T) {
	r := runModel(t, "v2-nat", 2*time.Minute)
	requireGreen(t, r,
		"v2-nat/cone-pairs",          // every pair of full, address- and port-restricted cone: direct
		"v2-nat/symmetric-initiator", // direct against full cone and address-restricted, relay against port-restricted and symmetric
		"v2-nat/symmetric-responder", // never direct (the offered port is the relay's): relay
		"v2-nat/no-port-spray",       // only the offered ports are ever written to, within the datagram budget
	)
}
