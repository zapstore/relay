package control

import (
	"fmt"
)

type Config struct {
	// Socket is the relay.sock path. Default is relay.sock in the data directory.
	Socket string `env:"RELAY_SOCKET_PATH"`

	// AdminPubkeys may sign write commands on relay.sock. Lowercase hex.
	// Load sets it from ADMIN_PUBKEYS.
	AdminPubkeys []string
}

func NewConfig() Config {
	return Config{}
}

func (c Config) Validate() error {
	return nil
}

func (c Config) String() string {
	socket := c.Socket
	if socket == "" {
		socket = "[data directory]/relay.sock"
	}
	return fmt.Sprintf("Control:\n"+
		"\tSocket: %s\n",
		socket)
}
