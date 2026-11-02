package catalog

import (
	"context"
	"strings"
	"time"
)

// Apps reads the live relay database and returns resolved listings whose app id contains id.
// pictures maps each author's pubkey to the kind 0 picture URL.
// signers maps an app id to the pubkey of its active C1 proof. An app with no
// active proof, including one whose listing the catalog key signed, is absent.
func Apps(ctx context.Context, dbPath, id string) ([]Listing, map[string]string, map[string]string, error) {
	raw, err := LoadEvents(ctx, dbPath, "", "")
	if err != nil {
		return nil, nil, nil, err
	}
	state := Resolve(raw, "")
	var listings []Listing
	authors := map[string]struct{}{}
	for _, listing := range state.Listings {
		if !strings.Contains(listing.AppID, id) {
			continue
		}
		listings = append(listings, listing)
		if listing.App.PubKey != "" {
			authors[listing.App.PubKey] = struct{}{}
		}
	}
	pictures := map[string]string{}
	for _, profile := range state.Profiles {
		if _, ok := authors[profile.Pubkey]; ok && profile.Picture != "" {
			pictures[profile.Pubkey] = profile.Picture
		}
	}
	now := time.Now()
	signers := map[string]string{}
	for _, listing := range listings {
		if signer := C1Signer(listing, state.Proofs, now); signer != "" {
			signers[listing.AppID] = signer
		}
	}
	return listings, pictures, signers, nil
}
