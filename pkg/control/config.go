package control

import (
	"fmt"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

type Config struct {
	// Socket is the relay.sock path. Default is relay.sock in the data directory.
	Socket string `env:"RELAY_SOCKET"`

	// AdminPubkeys may sign write commands on relay.sock. Hex pubkeys.
	// Empty falls back to DASHBOARD_ADMIN_PUBKEYS.
	AdminPubkeys []string `env:"ADMIN_PUBKEYS"`
}

func NewConfig() Config {
	return Config{}
}

func (c Config) Validate() error {
	for _, pk := range c.AdminPubkeys {
		if pk != strings.ToLower(pk) || !nostr.IsValidPublicKey(pk) {
			return fmt.Errorf("admin pubkey must be lowercase hex: %q", pk)
		}
	}
	return nil
}

func (c Config) String() string {
	socket := c.Socket
	if socket == "" {
		socket = "[data directory]/relay.sock"
	}
	return fmt.Sprintf("Control:\n"+
		"\tSocket: %s\n"+
		"\tAdmin Pubkeys: %v\n",
		socket, c.AdminPubkeys)
}
