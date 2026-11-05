package portmap

import (
	"testing"
)

func FuzzParsers(f *testing.F) {
	f.Add([]byte{0, 128, 0, 0, 0, 0, 3, 232, 8, 8, 4, 4})
	f.Add([]byte{2, 0x81, 0, 0, 0, 0, 14, 16, 0, 0, 3, 233, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add(make([]byte, 60))
	f.Add([]byte("<root><device><serviceList><service><serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType><controlURL>/c</controlURL></service></serviceList></device></root>"))
	f.Fuzz(func(t *testing.T, b []byte) {
		parsePMPExternal(b)
		parsePMPMap(b, pmpOpUDP, 4433)
		if h, ok, err := parsePCPHeader(b, pcpOpMap); ok && err == nil {
			parsePCPMap(b, h, [pcpNonceLen]byte{}, 17, 4433)
		}
		parsePCPHeader(b, pcpOpAnnounce)
		unsupportedByNATPMPRouter(b)
		findWANService(b)
		faultCode(b)
	})
}
