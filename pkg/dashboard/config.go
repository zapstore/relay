package dashboard

import (
	"fmt"
	"net"
)

type Config struct {
	// Address is the address the dashboard HTTP server listens on. Default is "localhost:3337".
	Address string `env:"DASHBOARD_ADDRESS"`
}

func NewConfig() Config {
	return Config{
		Address: "localhost:3337",
	}
}

func (c Config) Validate() error {
	if _, err := net.ResolveTCPAddr("tcp", c.Address); err != nil {
		return fmt.Errorf("invalid address %q: %w", c.Address, err)
	}
	return nil
}

func (c Config) String() string {
	return fmt.Sprintf("Dashboard:\n"+
		"\tAddress: %s\n",
		c.Address)
}
