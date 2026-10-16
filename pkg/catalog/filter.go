package catalog

import (
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

// matching keeps listings whose app id contains filter, plus the events those
// listings need. An empty filter returns s unchanged.
// Kept with the listings: certificate proofs, publisher profiles, stacks whose
// apps all match, and the catalog relay list. The relay list is not an app event.
func (s State) matching(filter string) State {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return s
	}
	var listings []Listing
	authors := map[string]struct{}{}
	certs := map[string]struct{}{}
	for _, listing := range s.Listings {
		if !strings.Contains(listing.AppID, filter) {
			continue
		}
		listings = append(listings, listing)
		if listing.App.PubKey != "" {
			authors[listing.App.PubKey] = struct{}{}
		}
		if signer, ancestors, ok := listingChain(listing); ok {
			certs[signer] = struct{}{}
			for _, ancestor := range ancestors {
				certs[ancestor] = struct{}{}
			}
		}
	}
	var proofs []Proof
	for _, proof := range s.Proofs {
		if _, ok := certs[strings.ToLower(proof.D)]; !ok {
			continue
		}
		proofs = append(proofs, proof)
		if proof.Pubkey != "" {
			authors[proof.Pubkey] = struct{}{}
		}
	}
	var profiles []Profile
	for _, profile := range s.Profiles {
		if _, ok := authors[profile.Pubkey]; ok {
			profiles = append(profiles, profile)
		}
	}
	var stacks []Stack
	for _, stack := range s.Stacks {
		if stackMatches(stack.Event, filter) {
			stacks = append(stacks, stack)
		}
	}
	return State{
		Listings: listings,
		Proofs:   proofs,
		Stacks:   stacks,
		Profiles: profiles,
		Relays:   s.Relays,
		Notices:  s.Notices,
	}
}

// stackMatches reports whether every app named by the stack contains filter.
func stackMatches(event nostr.Event, filter string) bool {
	parsed, err := events.ParseStack(&event)
	if err != nil || len(parsed.Apps) == 0 {
		return false
	}
	for _, app := range parsed.Apps {
		if !strings.Contains(app.AppID, filter) {
			return false
		}
	}
	return true
}
