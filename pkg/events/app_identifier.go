package events

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

// AppIdentifier is a parsed app identifier from the stringified format "32267:<pubkey>:<app_id>"
type AppIdentifier struct {
	Pubkey string
	AppID  string
}

func (a AppIdentifier) String() string {
	return "32267:" + a.Pubkey + ":" + a.AppID
}

func (a AppIdentifier) Validate() error {
	if a.Pubkey == "" || !nostr.IsValidPublicKey(a.Pubkey) {
		return errors.New("invalid pubkey app identifier")
	}
	if a.AppID == "" {
		return fmt.Errorf("invalid app ID in app identifier: %s", a.AppID)
	}
	return nil
}

// ParseAppIdentifier parses a stringified app identifier into an AppIdentifier struct.
func ParseAppIdentifier(s string) (AppIdentifier, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return AppIdentifier{}, fmt.Errorf("invalid app identifier format")
	}
	if parts[0] != "32267" {
		return AppIdentifier{}, fmt.Errorf("invalid app identifier format: invalid kind")
	}
	return AppIdentifier{
		Pubkey: parts[1],
		AppID:  parts[2],
	}, nil
}

// ValidateAppIdentifier validates a stringified app identifier.
func ValidateAppIdentifier(s string) error {
	id, err := ParseAppIdentifier(s)
	if err != nil {
		return err
	}
	return id.Validate()
}
