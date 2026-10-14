package config

import "testing"

func TestSeedNodesFromTheBuild(t *testing.T) {
	old := SeedNodes
	defer func() { SeedNodes = old }()
	SeedNodes = " seed1.example.org:4242/aa , ,seed2.example.org:4242/bb"
	got := DefaultConfig("").Network.BootstrapNodes
	if len(got) != len(DefaultBootstrapNodes)+2 || got[len(got)-2] != "seed1.example.org:4242/aa" || got[len(got)-1] != "seed2.example.org:4242/bb" {
		t.Fatalf("bootstrap nodes: %v", got)
	}
}
