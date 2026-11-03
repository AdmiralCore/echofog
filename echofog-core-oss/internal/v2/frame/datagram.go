package frame

// A datagram is type(varint) ‖ payload; its length is the datagram length (wire.md §6.5).

// AppendDatagram appends a datagram of type typ.
func AppendDatagram(b []byte, typ uint64, payload []byte) []byte {
	return append(AppendVarint(b, typ), payload...)
}

// ParseDatagram splits a datagram into its type and payload (not copied). Unknown types are not
// an error here; receivers drop and count them (registries.md §2).
func ParseDatagram(d []byte) (uint64, []byte, error) {
	typ, n, err := ParseVarint(d)
	if err != nil {
		return 0, nil, err
	}
	return typ, d[n:], nil
}
