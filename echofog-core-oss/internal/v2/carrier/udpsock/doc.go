// Package udpsock is the UDP socket layer of the v2 carriers (docs/protocol/v2/platforms.md
// §1.2, §2): one net.PacketConn over a UDP socket that sends with GSO, receives with recvmmsg,
// carries the ECN control messages, and falls back to one call per datagram when the kernel, the
// SELinux policy or the flag udp=restricted says no.
//
// The package changes no byte on the wire (platforms.md §3). What is batched here is local: the
// kernel cuts a GSO train into the same datagrams that single writes would put on the wire, and a
// socket that cannot batch sends and receives them one at a time.
//
// Rules from platforms.md §1.2 and §2:
//
//	udp=full        GSO, recvmmsg and ECN are tried. The first failure of a batched operation
//	                is permanent for that socket: a kernel or an SELinux policy that refuses
//	                GSO or recvmmsg keeps refusing it, and paying for the failing system call on
//	                every datagram is worse than one plain call per datagram. A refused GSO
//	                train is retried as single writes before the error reaches the caller, so
//	                the fallback loses no datagram.
//	udp=restricted  no GSO, no recvmmsg, no ECN from the start (Options.Restricted).
//	protect(fd)     the app's callback runs before the socket binds, connects or sees its first
//	                datagram (platforms.md §1.1), so a VpnService keeps the core's traffic out
//	                of its own tunnel.
//
// The socket is meant to be handed to the v2 transports. quic-go turns its fast path on only
// when the connection offers ReadMsgUDP, WriteMsgUDP, SyscallConn and either ReadBatch or
// net.Conn (third_party/quic-go/sys_conn.go). Conn offers all of them, so GSO, ECN and batch
// reads survive under noise3 and QUIC; TransportConn returns a plain view of the socket in
// restricted mode, where quic-go must stay on single calls too.
//
// Build tags:
//
//	linux                             GSO (UDP_SEGMENT) and recvmmsg. The linux tag also
//	                                  builds for android, as platforms.md §1.2 requires: an
//	                                  Android kernel or SELinux policy may refuse either call,
//	                                  and the socket then falls back at run time. No Linux call
//	                                  here breaks the Android build, so no file needs
//	                                  `linux && !android`.
//	(linux || darwin || freebsd) && !ios
//	                                  ECN control messages (x/sys/unix). iOS satisfies the
//	                                  darwin tag but takes the fallback: decisions.md T2.2
//	                                  leaves it without control messages.
//	everything else                   one ReadFrom/WriteTo per datagram, no control messages
//	                                  (the oob_other.go fallback of platforms.md §1.2). This
//	                                  includes Windows and iOS.
package udpsock
