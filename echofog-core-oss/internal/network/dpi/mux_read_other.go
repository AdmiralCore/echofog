//go:build !linux

package dpi

// readLoop reads the shared socket one datagram at a time. quic-go reads no control messages on
// Windows, and on macOS and FreeBSD it reads one datagram per call anyway.
func (m *MuxedConn) readLoop() {
	for {
		bufPtr := packetPool.Get().(*[]byte)
		n, addrPort, err := m.conn.ReadFromUDPAddrPort((*bufPtr)[:maxDatagram])
		if err != nil {
			packetPool.Put(bufPtr)
			return
		}
		m.dispatch(bufPtr, n, 0, addrPort)
	}
}
