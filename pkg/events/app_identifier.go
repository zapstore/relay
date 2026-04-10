package events

import (
	"fmt"
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
	return AddressableRef{Kind: KindApp, Pubkey: a.Pubkey, DTag: a.AppID}.Validate()
}

// ParseAppIdentifier parses a stringified app identifier into an AppIdentifier struct.
func ParseAppIdentifier(s string) (AppIdentifier, error) {
	ref, err := ParseAddressableRef(s)
	if err != nil {
		return AppIdentifier{}, err
	}
	if ref.Kind != KindApp {
		return AppIdentifier{}, fmt.Errorf("invalid app identifier: expected kind %d, got %d", KindApp, ref.Kind)
	}
	return AppIdentifier{
		Pubkey: ref.Pubkey,
		AppID:  ref.DTag,
	}, nil
}

// ValidateAppIdentifier validates a stringified app identifier.
func ValidateAppIdentifier(s string) error {
	ref, err := ParseAddressableRef(s)
	if err != nil {
		return err
	}

	if ref.Kind != KindApp {
		return fmt.Errorf("invalid app identifier: expected kind %d, got %d", KindApp, ref.Kind)
	}
	return ref.Validate()
}
