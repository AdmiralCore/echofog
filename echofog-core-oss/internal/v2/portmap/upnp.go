package portmap

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// maxUPnPBody bounds every UPnP answer read; real descriptions are a few KB.
	maxUPnPBody = 256 << 10
	// maxLocations bounds the devices tried from one SSDP search.
	maxLocations = 3
	upnpTimeout  = 5 * time.Second
	// maxPortTries bounds the conflicting-port retries of one mapping.
	maxPortTries = 4

	errOnlyPermanent = 725
)

// A UPnP device is talked to only if it is the default gateway: any host on the LAN can answer
// an SSDP search and name any URL, and the router's control URL must not lead elsewhere.

type upnpClient struct {
	m       *Manager
	control *url.URL
	service string
	local   netip.Addr
}

func (m *Manager) discoverUPnP(ctx context.Context, gw, local netip.Addr) (gateway, error) {
	locs, err := m.cfg.SSDP(ctx)
	if err != nil {
		return nil, err
	}
	var lastErr error = errUnsupported
	tried := 0
	for _, loc := range locs {
		if tried >= maxLocations || ctx.Err() != nil {
			break
		}
		u, err := url.Parse(loc)
		if err != nil || u.Scheme != "http" || u.User != nil {
			continue
		}
		if a, err := netip.ParseAddr(u.Hostname()); err != nil || a.Unmap() != gw {
			continue
		}
		tried++
		c, err := m.upnpFromDescription(ctx, u, local)
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (m *Manager) upnpFromDescription(ctx context.Context, desc *url.URL, local netip.Addr) (gateway, error) {
	body, err := m.upnpDo(ctx, http.MethodGet, desc.String(), "", nil)
	if err != nil {
		return nil, err
	}
	svcType, ctrl, err := findWANService(body)
	if err != nil {
		return nil, err
	}
	cu, err := desc.Parse(ctrl)
	if err != nil || cu.Scheme != "http" || cu.Host != desc.Host || cu.User != nil {
		return nil, errors.New("portmap: upnp: control URL is not on the gateway")
	}
	c := &upnpClient{m: m, control: cu, service: svcType, local: local}
	if _, err := c.externalAddr(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

type upnpService struct {
	Type    string `xml:"serviceType"`
	Control string `xml:"controlURL"`
}

type upnpDevice struct {
	Services []upnpService `xml:"serviceList>service"`
	Devices  []upnpDevice  `xml:"deviceList>device"`
}

type upnpRoot struct {
	Device upnpDevice `xml:"device"`
}

// findWANService returns the first WANIPConnection or WANPPPConnection service of the device
// tree.
func findWANService(desc []byte) (svcType, control string, err error) {
	var root upnpRoot
	if err := xml.Unmarshal(desc, &root); err != nil {
		return "", "", fmt.Errorf("portmap: upnp: description: %w", err)
	}
	var walk func(d *upnpDevice) bool
	walk = func(d *upnpDevice) bool {
		for _, s := range d.Services {
			t := strings.TrimSpace(s.Type)
			if strings.HasPrefix(t, "urn:schemas-upnp-org:service:WANIPConnection:") ||
				strings.HasPrefix(t, "urn:schemas-upnp-org:service:WANPPPConnection:") {
				svcType, control = t, strings.TrimSpace(s.Control)
				return control != ""
			}
		}
		for i := range d.Devices {
			if walk(&d.Devices[i]) {
				return true
			}
		}
		return false
	}
	if !walk(&root.Device) {
		return "", "", errors.New("portmap: upnp: no WAN connection service")
	}
	return svcType, control, nil
}

func (c *upnpClient) name() string { return "UPnP" }

// upnpDo runs one request with a size-bounded answer. A non-2xx answer is returned with its body
// and a *upnpFault when it carries a UPnP error.
func (m *Manager) upnpDo(ctx context.Context, method, target, soapAction string, payload []byte) ([]byte, error) {
	ctx, cancel := m.withTimeout(ctx, upnpTimeout)
	defer cancel()
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, err
	}
	if soapAction != "" {
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		req.Header.Set("SOAPAction", soapAction)
	}
	resp, err := m.cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUPnPBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxUPnPBody {
		return nil, errors.New("portmap: upnp: answer too large")
	}
	if resp.StatusCode/100 != 2 {
		return body, &upnpFault{status: resp.StatusCode, code: faultCode(body)}
	}
	return body, nil
}

type upnpFault struct {
	status int
	code   int
}

func (e *upnpFault) Error() string {
	return fmt.Sprintf("portmap: upnp: HTTP %d, error %d", e.status, e.code)
}

// faultCode finds <errorCode> in a SOAP fault; 0 if there is none.
func faultCode(body []byte) int {
	d := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := d.Token()
		if err != nil {
			return 0
		}
		if s, ok := tok.(xml.StartElement); ok && s.Name.Local == "errorCode" {
			var v string
			if d.DecodeElement(&v, &s) != nil {
				return 0
			}
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
}

type soapArg struct{ name, value string }

func (c *upnpClient) soap(ctx context.Context, action string, args ...soapArg) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%s xmlns:u="%s">`, action, c.service)
	for _, a := range args {
		fmt.Fprintf(&b, "<%s>", a.name)
		_ = xml.EscapeText(&b, []byte(a.value))
		fmt.Fprintf(&b, "</%s>", a.name)
	}
	fmt.Fprintf(&b, `</u:%s></s:Body></s:Envelope>`, action)
	return c.m.upnpDo(ctx, http.MethodPost, c.control.String(), `"`+c.service+"#"+action+`"`, b.Bytes())
}

func (c *upnpClient) externalAddr(ctx context.Context) (netip.Addr, error) {
	body, err := c.soap(ctx, "GetExternalIPAddress")
	if err != nil {
		return netip.Addr{}, err
	}
	d := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := d.Token()
		if err != nil {
			return netip.Addr{}, errors.New("portmap: upnp: no external address in the answer")
		}
		if s, ok := tok.(xml.StartElement); ok && s.Name.Local == "NewExternalIPAddress" {
			var v string
			if err := d.DecodeElement(&v, &s); err != nil {
				return netip.Addr{}, err
			}
			a, err := netip.ParseAddr(strings.TrimSpace(v))
			if err != nil {
				return netip.Addr{}, err
			}
			return a.Unmap(), nil
		}
	}
}

func upnpProto(p Proto) (string, error) {
	switch p {
	case UDP:
		return "UDP", nil
	case TCP:
		return "TCP", nil
	}
	return "", ErrBadPort
}

func (c *upnpClient) mapPort(ctx context.Context, p Proto, internal, suggested uint16, lifetime time.Duration) (mapResult, error) {
	proto, err := upnpProto(p)
	if err != nil {
		return mapResult{}, err
	}
	// the router shows this and may report it onward: it names the protocol and port only
	desc := proto + " " + strconv.Itoa(int(internal))
	lease := uint32(lifetime / time.Second)
	ext := suggested
	for try := 0; try < maxPortTries; try++ {
		_, err = c.soap(ctx, "AddPortMapping",
			soapArg{"NewRemoteHost", ""},
			soapArg{"NewExternalPort", strconv.Itoa(int(ext))},
			soapArg{"NewProtocol", proto},
			soapArg{"NewInternalPort", strconv.Itoa(int(internal))},
			soapArg{"NewInternalClient", c.local.String()},
			soapArg{"NewEnabled", "1"},
			soapArg{"NewPortMappingDescription", desc},
			soapArg{"NewLeaseDuration", strconv.FormatUint(uint64(lease), 10)})
		if err == nil {
			break
		}
		var f *upnpFault
		if !errors.As(err, &f) {
			return mapResult{}, err
		}
		if f.code == errOnlyPermanent && lease != 0 {
			lease = 0 // the router keeps no timed leases; we renew anyway
			continue
		}
		// the port is taken, or the router refuses this one: try another
		ext = uint16(10000 + int(c.m.cfg.Rand()*float64(65535-10000)))
	}
	if err != nil {
		return mapResult{}, err
	}
	addr, err := c.externalAddr(ctx)
	if err != nil {
		return mapResult{}, err
	}
	return mapResult{extPort: ext, addr: addr, lifetime: time.Duration(lease) * time.Second}, nil
}

func (c *upnpClient) unmap(ctx context.Context, p Proto, _, ext uint16) error {
	proto, err := upnpProto(p)
	if err != nil {
		return err
	}
	_, err = c.soap(ctx, "DeletePortMapping",
		soapArg{"NewRemoteHost", ""},
		soapArg{"NewExternalPort", strconv.Itoa(int(ext))},
		soapArg{"NewProtocol", proto})
	return err
}
