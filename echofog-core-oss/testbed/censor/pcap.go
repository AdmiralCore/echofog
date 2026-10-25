// Package censor looks at captured traffic the way a censor's DPI does: what protocol each flow
// shows on the wire, the SNI it names, whether its first packet passes the "fully encrypted"
// heuristic the Great Firewall applies, whether a QUIC packet is real QUIC (it decrypts the
// Initial, as GFW and TSPU do), and the shape of the flow. It reads classic pcap files as tcpdump
// writes them, without libpcap.
package censor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"time"
)

// packet is one captured IPv4 TCP or UDP packet.
type packet struct {
	at       time.Time
	proto    uint8 // 6 TCP, 17 UDP
	src, dst netip.AddrPort
	syn      bool
	payload  []byte
	size     int // IP packet length
}

const (
	linkEthernet = 1
	linkRaw      = 101
	linkLinuxSLL = 113
)

// readPcap reads every IPv4 TCP/UDP packet of a classic pcap file.
func readPcap(path string) ([]packet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var hdr [24]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, fmt.Errorf("pcap header: %w", err)
	}
	var bo binary.ByteOrder
	nano := false
	switch binary.LittleEndian.Uint32(hdr[:4]) {
	case 0xa1b2c3d4:
		bo = binary.LittleEndian
	case 0xa1b23c4d:
		bo, nano = binary.LittleEndian, true
	case 0xd4c3b2a1:
		bo = binary.BigEndian
	case 0x4d3cb2a1:
		bo, nano = binary.BigEndian, true
	default:
		return nil, errors.New("not a classic pcap file (pcapng is not read)")
	}
	link := bo.Uint32(hdr[20:24])

	var out []packet
	var rec [16]byte
	for {
		if _, err := io.ReadFull(f, rec[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return out, nil
			}
			return out, err
		}
		sec, frac := bo.Uint32(rec[0:4]), bo.Uint32(rec[4:8])
		incl := bo.Uint32(rec[8:12])
		if incl > 1<<18 {
			return out, errors.New("pcap record too large")
		}
		data := make([]byte, incl)
		if _, err := io.ReadFull(f, data); err != nil {
			return out, nil // a capture cut off mid-record
		}
		ns := int64(frac) * 1000
		if nano {
			ns = int64(frac)
		}
		if p, ok := decode(link, data); ok {
			p.at = time.Unix(int64(sec), ns)
			out = append(out, p)
		}
	}
}

func decode(link uint32, b []byte) (packet, bool) {
	switch link {
	case linkEthernet:
		if len(b) < 14 {
			return packet{}, false
		}
		et := binary.BigEndian.Uint16(b[12:14])
		b = b[14:]
		if et == 0x8100 && len(b) >= 4 { // VLAN
			et = binary.BigEndian.Uint16(b[2:4])
			b = b[4:]
		}
		if et != 0x0800 {
			return packet{}, false
		}
	case linkLinuxSLL:
		if len(b) < 16 || binary.BigEndian.Uint16(b[14:16]) != 0x0800 {
			return packet{}, false
		}
		b = b[16:]
	case linkRaw:
	default:
		return packet{}, false
	}
	return decodeIPv4(b)
}

func decodeIPv4(b []byte) (packet, bool) {
	if len(b) < 20 || b[0]>>4 != 4 {
		return packet{}, false
	}
	ihl := int(b[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(b[2:4]))
	if ihl < 20 || total < ihl || len(b) < ihl {
		return packet{}, false
	}
	if total > len(b) {
		total = len(b) // snap length
	}
	if binary.BigEndian.Uint16(b[6:8])&0x1fff != 0 {
		return packet{}, false // a later fragment
	}
	src, _ := netip.AddrFromSlice(b[12:16])
	dst, _ := netip.AddrFromSlice(b[16:20])
	p := packet{proto: b[9], size: total}
	l4 := b[ihl:total]
	switch p.proto {
	case 6:
		if len(l4) < 20 {
			return packet{}, false
		}
		off := int(l4[12]>>4) * 4
		if off < 20 || off > len(l4) {
			return packet{}, false
		}
		p.src = netip.AddrPortFrom(src, binary.BigEndian.Uint16(l4[0:2]))
		p.dst = netip.AddrPortFrom(dst, binary.BigEndian.Uint16(l4[2:4]))
		p.syn = l4[13]&0x02 != 0 && l4[13]&0x10 == 0
		p.payload = l4[off:]
	case 17:
		if len(l4) < 8 {
			return packet{}, false
		}
		p.src = netip.AddrPortFrom(src, binary.BigEndian.Uint16(l4[0:2]))
		p.dst = netip.AddrPortFrom(dst, binary.BigEndian.Uint16(l4[2:4]))
		p.payload = l4[8:]
	default:
		return packet{}, false
	}
	return p, true
}
