package main

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// listenSpec is one --listen entry: where the carrier binds and what the card advertises. The
// lab CLI binds wildcard or a container address and advertises a dialable IP literal, because
// noise3 refuses to resolve DNS names locally (T2.4, N-4).
type listenSpec struct {
	Suite         uint32
	Bind          string
	AdvertiseHost string
	Port          uint32
}

// parseListen parses "suite=HOST:PORT[,suite=HOST:PORT]". cover is the --cover value: it belongs
// to reality2-quic, which waits for T2.6b, so it is refused until then.
func parseListen(spec, cover string) ([]listenSpec, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		if cover != "" {
			return nil, fmt.Errorf("--cover %q is only used by reality2-quic (T2.6b)", cover)
		}
		return nil, nil
	}
	var out []listenSpec
	sawReality := false
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("empty listener in %q", spec)
		}
		name, addr, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("listener %q: want suite=host:port", entry)
		}
		name = strings.TrimSpace(name)
		suite, err := suiteOf(name)
		if err != nil {
			return nil, err
		}
		host, portText, err := net.SplitHostPort(strings.TrimSpace(addr))
		if err != nil {
			return nil, fmt.Errorf("listener %q: want host:port: %w", entry, err)
		}
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || port == 0 {
			return nil, fmt.Errorf("listener %q: port %q", entry, portText)
		}
		if suite == objects.SuiteReality2QUIC {
			sawReality = true
			continue // reported below with the T2.6b message
		}
		advertise, err := advertisedHost(host)
		if err != nil {
			return nil, fmt.Errorf("listener %q: %w", entry, err)
		}
		bind := net.JoinHostPort(host, portText)
		out = append(out, listenSpec{Suite: suite, Bind: bind, AdvertiseHost: advertise, Port: uint32(port)})
	}
	if sawReality {
		return nil, fmt.Errorf("reality2-quic listeners wait for T2.6b (the reproducible cover); use noise3")
	}
	if cover != "" {
		return nil, fmt.Errorf("--cover %q is only used by reality2-quic (T2.6b)", cover)
	}
	return out, nil
}

func suiteOf(name string) (uint32, error) {
	switch name {
	case "noise3":
		return objects.SuiteNoise3, nil
	case "reality2-quic":
		return objects.SuiteReality2QUIC, nil
	case "reality2-tcp":
		return objects.SuiteReality2TCP, nil
	case "ws2":
		return objects.SuiteWS2, nil
	default:
		return 0, fmt.Errorf("suite %q: want noise3, reality2-quic, reality2-tcp or ws2", name)
	}
}

// advertisedHost turns a bind host into the address written into the card. A wildcard binds all
// interfaces but is not dialable, so a lab card advertises loopback; a DNS name is refused
// because noise3 dials only IP literals without an injected resolver.
func advertisedHost(host string) (string, error) {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "127.0.0.1", nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", fmt.Errorf("host %q: want an IP literal or a wildcard bind", host)
	}
	return ip.Unmap().String(), nil
}
