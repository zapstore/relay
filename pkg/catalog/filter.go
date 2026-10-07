package catalog

import (
	"slices"
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

// profilesFor is every profile, or only authors of listings selected by filter.
func profilesFor(st State, filter string) []Profile {
	if filter == "" {
		return st.Profiles
	}
	want := map[string]struct{}{}
	for _, listing := range st.Listings {
		if strings.Contains(listing.AppID, filter) && listing.App.PubKey != "" {
			want[listing.App.PubKey] = struct{}{}
		}
	}
	var out []Profile
	for _, profile := range st.Profiles {
		if _, ok := want[profile.Pubkey]; ok {
			out = append(out, profile)
		}
	}
	return out
}

// onlyMatching keeps diff entries whose app id contains filter.
// It does not add listings that were not already in the diff.
// Profiles and avatars stay when they belong to an author of a matching listing.
func onlyMatching(d Diff, next State, filter string) Diff {
	authors := map[string]struct{}{}
	for _, listing := range next.Listings {
		if !strings.Contains(listing.AppID, filter) || listing.App.PubKey == "" {
			continue
		}
		authors[listing.App.PubKey] = struct{}{}
	}
	var apps []string
	for _, id := range d.Apps {
		if strings.Contains(id, filter) {
			apps = append(apps, id)
		}
	}
	slices.Sort(apps)

	var ev []nostr.Event
	for _, event := range d.Events {
		if eventMatches(event, filter, authors) {
			ev = append(ev, event)
		}
	}
	var deletes []string
	for _, id := range d.AppDeletes {
		if strings.Contains(id, filter) {
			deletes = append(deletes, id)
		}
	}
	var avatars []string
	for _, name := range d.Avatars {
		pub := strings.TrimSuffix(name, ".webp")
		if _, ok := authors[pub]; ok {
			avatars = append(avatars, name)
		}
	}
	d.Apps = apps
	d.Events = ev
	d.AppDeletes = deletes
	d.Avatars = avatars
	d.Coords = nil
	return d
}

func eventMatches(event nostr.Event, filter string, authors map[string]struct{}) bool {
	switch event.Kind {
	case events.KindApp:
		id, _ := events.Find(event.Tags, "d")
		return strings.Contains(id, filter)
	case events.KindRelease, events.KindAsset:
		id, _ := events.Find(event.Tags, "i")
		return strings.Contains(id, filter)
	case events.KindProfile:
		_, ok := authors[event.PubKey]
		return ok
	default:
		return false
	}
}
