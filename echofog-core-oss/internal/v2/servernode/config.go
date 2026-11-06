// Package servernode is the headless server node of EchoFog v2 (T12.4a): the profile on disk, the
// configuration, the operator API with tokens and the `echofog-node v2` commands. The protocol is
// the runtime of internal/v2/node; this package only wires a server profile around it.
//
// A server is exposed to the internet, so the defaults are closed: the operator API listens on a
// loopback IP literal only and answers only to bearer tokens and loopback Host names; there is no
// debug or profiling endpoint; the CDN clock check stays off (the runtime has no HTTP client
// unless one with a uTLS hello is supplied); keys live in the profile directory with mode 0600
// and in no log.
package servernode

import (
	"errors"
	"time"
)

// ErrConfig is a configuration that is refused.
var ErrConfig = errors.New("servernode: bad configuration")

// MaxConfigSize bounds the configuration file.
const MaxConfigSize = 64 << 10

// ListenSpec is one carrier listener of the node.
type ListenSpec struct {
	// Suite is the carrier suite; this build serves noise3 (reality2 and ws2 wait for T2.6b-3).
	Suite string `json:"suite"`
	// Bind is the local host:port. Advertise is the IP literal written into the card.
	Bind      string `json:"bind"`
	Advertise string `json:"advertise"`
}

// OperatorSpec configures the operator API.
type OperatorSpec struct {
	// Addr is a loopback IP literal with a port ("127.0.0.1:0" picks a free one).
	Addr string `json:"addr"`
}

// Config is server.json in the data directory.
type Config struct {
	Version int          `json:"version"`
	Listen  []ListenSpec `json:"listen"`
	// InviteListen is the separate listener that invitations point to. Without it invitations are
	// off.
	InviteListen *ListenSpec `json:"invite_listen,omitempty"`
	// Roles are relay, exit, mailbox and dht-server.
	Roles []string `json:"roles"`
	// Cover is host:port of the site the camouflaged suites imitate. Without a camouflaged listener
	// the node has no public card (D37) and tells the operator.
	Cover    string       `json:"cover,omitempty"`
	Operator OperatorSpec `json:"operator"`
}

// DefaultConfig is a server with the default roles and the operator API on loopback.
func DefaultConfig() Config {
	return Config{Version: 1, Roles: []string{"relay", "mailbox", "dht-server"}, Operator: OperatorSpec{Addr: "127.0.0.1:0"}}
}

// ParseConfig reads and validates a configuration. Unknown fields, trailing data and anything
// over MaxConfigSize are refused.
func ParseConfig(b []byte) (*Config, error) { return nil, errors.New("servernode: not implemented") }

// Validate checks a configuration.
func (c *Config) Validate() error { return errors.New("servernode: not implemented") }

// ParseDuration reads a term such as "30d", "12h", "90m" or "1d12h". It must be positive.
func ParseDuration(string) (time.Duration, error) {
	return 0, errors.New("servernode: not implemented")
}
