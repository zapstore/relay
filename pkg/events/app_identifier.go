package events

import (
	"fmt"
)

// AppIdentifier is a parsed app identifier from the stringified format "32267:<pubkey>:<app_id>"
type AppIdentifier struct {
	Pubkey string
	AppID  string
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
