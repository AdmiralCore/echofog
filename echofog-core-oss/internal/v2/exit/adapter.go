package exit

// udpAdapter bridges one v2 UDP tunnel to the v1 exit core's framed UDP relay.
type udpAdapter struct {
	id     uint32
	req    uint64
	inbox  chan udpReply
}

type udpReply struct {
	addr string
	data []byte
}
