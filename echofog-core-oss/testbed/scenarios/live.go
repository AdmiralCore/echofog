package scenarios

// The live descriptions of the scenarios: what runner starts on the Docker stand and what the stand
// or the efv2 CLI must have. The compose services are defined in testbed/docker-compose.yml under
// the "v2" profile; testbed/scenarios/live_test.go checks that they exist there.

var liveNAT = Live{
	Command:  "go run ./testbed/runner -scenario v2-nat",
	Services: []string{"gw_a", "gw_b", "gw_c", "v2r", "v2na", "v2nb", "v2nc"},
	Needs: []string{
		"efv2 run --relay: a relay role on the public node v2r",
		"efv2 echo reports the path kind (direct or relay) and the NAT class the node learned",
		"NAT_B=symmetric-narrow for the symmetric-narrow row; the owner-run birthday punch is not part of this scenario",
	},
}

var livePaths = Live{
	Command:  "go run ./testbed/runner -scenario v2-paths",
	Services: []string{"gw_b", "v2pa", "v2pb"},
	Needs: []string{
		"efv2 run with two noise3 listeners (--listen noise3=A,noise3=B) on v2pb",
		"efv2 echo --paths 2 --cut: a second path and a router cut (docker exec gw_b iptables) in the middle of the stream",
	},
}

var liveHidden = Live{
	Command:  "go run ./testbed/runner -scenario v2-hidden",
	Services: []string{"v2h", "v2ha", "v2hb"},
	Needs: []string{
		"efv2 run --access hidden and efv2 link --invite: an invitation with an access term (efv2 link exists, hidden mode does not)",
		"efv2 link join on v2ha; v2hb has no link and must get silence",
	},
}

var liveMessaging = Live{
	Command:  "go run ./testbed/runner -scenario v2-messaging",
	Services: []string{"v2ma", "v2mb", "v2mx"},
	Needs: []string{
		"efv2 chat send/recv/history: the chat service is in the runtime (Config.Chat) but the CLI has no command for it",
		"efv2 run --mailbox on the server node v2mx",
	},
}

var liveCall = Live{
	Command:  "go run ./testbed/runner -scenario v2-call",
	Services: []string{"gw_a", "gw_b", "gw_c", "v2r", "v2ca", "v2cc"},
	Needs: []string{
		"efv2 call: the call service is in the runtime, the CLI has no command; it must count voice and video frames sent and received",
		"NETEM_A / NETEM_C for the loss row (5 % loss, 150 ms)",
	},
}

var liveExit = Live{
	Command:  "go run ./testbed/runner -scenario v2-exit",
	Services: []string{"v2xe", "v2xc", "web"},
	Needs: []string{
		"efv2 run --exit on v2xe and efv2 exit fetch on v2xc: the exit client is in the runtime, the CLI has no command",
		"the lab web server (web) serves files with their SHA-256",
	},
}

var liveCensor = Live{
	Command:  "go run ./testbed/runner -scenario v2-censor -v2-carriers noise3,reality2-quic,reality2-tcp,ws2",
	Services: []string{"gw_b", "x1", "web"},
	Needs: []string{
		"efv2 run --listen reality2-quic, reality2-tcp and ws2 listeners with --cover: the CLI serves noise3 only today",
		"testbed/v2carrier modes for the three real carriers (the harness drives a synthetic stand-in for noise3 and none for the others)",
	},
}
