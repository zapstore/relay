package dashboard

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/nwt"
)

type Config struct {
	// Address is the address the dashboard HTTP server listens on. Default is "localhost:3337".
	Address string `env:"DASHBOARD_ADDRESS"`

	// Hostname is the hostname of the dashboard, used for Nostr Web Token auth (e.g. dashboard.zapstore.dev)
	Hostname string `env:"DASHBOARD_HOSTNAME"`

	// AllowedPubkeys is the list of the pubkeys that can access the dashboard.
	AllowedPubkeys []string `env:"DASHBOARD_ALLOWED_PUBKEYS"`
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
	if len(c.AllowedPubkeys) == 0 {
		return errors.New("allowed pubkeys is empty")
	}
	for _, pk := range c.AllowedPubkeys {
		if pk == "" || !nostr.IsValidPublicKey(pk) {
			return fmt.Errorf("allowed pubkey is invalid: %q", pk)
		}
	}
	return nil
}

func (c Config) String() string {
	return fmt.Sprintf("Dashboard:\n"+
		"\tAddress: %s\n"+
		"\tHostname: %s\n"+
		"\tAllowed Pubkeys: %v\n",
		c.Address, c.Hostname, c.AllowedPubkeys)
}

// authValidator implements [nwt.Validator]
type authValidator struct {
	Config
}

func (v authValidator) Validate(t nwt.Token) error {
	if t.ID == "" {
		return nwt.ErrEmptyID
	}
	if err := nwt.ValidateTimeClaims(t, time.Minute); err != nil {
		return err
	}
	if len(t.Audience) > 0 {
		if !slices.Contains(t.Audience, v.Hostname) {
			return fmt.Errorf("%w: it doesn't contain an exact match of %q", nwt.ErrInvalidAudience, v.Hostname)
		}
	}
	if !slices.Contains(v.AllowedPubkeys, t.Signer) {
		return fmt.Errorf("pubkey is not allowed: %q", t.Signer)
	}
	return nil
}
