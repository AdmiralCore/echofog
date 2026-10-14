package dht

type DhtType uint32

const (
	TypeKeyBundle DhtType = iota
	TypePeerData
	TypeContentHash
	TypePeerName
)
