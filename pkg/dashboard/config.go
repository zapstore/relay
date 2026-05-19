package dashboard

import (
	"errors"
	"fmt"
	"net"

	"github.com/nbd-wtf/go-nostr"
)

type Config struct {
	// Address is the address the dashboard HTTP server listens on. Default is "localhost:3337".
	Address string `env:"DASHBOARD_ADDRESS"`

	// Hostname is the hostname of the dashboard, used for Nostr Web Token auth (e.g. dashboard.zapstore.dev)
	Hostname string `env:"DASHBOARD_HOSTNAME"`

	// ViewerPubkeys is the list of the pubkeys that can access the dashboard.
	ViewerPubkeys []string `env:"DASHBOARD_VIEWER_PUBKEYS"`

	// AdminPubkeys is the list of the pubkeys that can access the dashboard admin panel.
	AdminPubkeys []string `env:"DASHBOARD_ADMIN_PUBKEYS"`
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
	if c.Hostname == "" {
		return errors.New("dashboard hostname is not set")
	}
	if len(c.AdminPubkeys) == 0 {
		return errors.New("admin pubkeys is empty")
	}
	for _, pk := range c.AdminPubkeys {
		if pk == "" || !nostr.IsValidPublicKey(pk) {
			return fmt.Errorf("admin pubkey is invalid: %q", pk)
		}
	}
	for _, pk := range c.ViewerPubkeys {
		if pk == "" || !nostr.IsValidPublicKey(pk) {
			return fmt.Errorf("viewer pubkey is invalid: %q", pk)
		}
	}
	return nil
}

func (c Config) String() string {
	return fmt.Sprintf("Dashboard:\n"+
		"\tAddress: %s\n"+
		"\tHostname: %s\n"+
		"\tViewer Pubkeys: %v\n"+
		"\tAdmin Pubkeys: %v\n",
		c.Address, c.Hostname, c.ViewerPubkeys, c.AdminPubkeys)
}
