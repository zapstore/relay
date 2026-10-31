package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/events"
)

const (
	// devPrefix apps stay signed by oldKey: it holds their C1 proof and stays the developer.
	devPrefix     = "dev.zapstore"
	relayHint     = "wss://relay.zapstore.dev"
	platformARM64 = "android-arm64-v8a"
	// kindFileMetadata is NIP-94, the v1 asset. No current release names one.
	kindFileMetadata = 1063
)

// listing is one current oldKey listing to re-sign.
type listing struct {
	AppID   string      `json:"app_id"`
	App     nostr.Event `json:"app"`
	Release nostr.Event `json:"release"`
	// Assets are every asset the release names, in its e tag order.
	Assets []nostr.Event `json:"assets"`
	// Ancestors replace an asset's apk_certificate_ancestor tags, by asset id.
	Ancestors map[string][]string `json:"ancestors,omitempty"`
}

// plan is what rotation deletes and re-signs.
type plan struct {
	Listings []listing     `json:"listings"`
	Stacks   []nostr.Event `json:"stacks"`
	// Profile is oldKey's kind 0. Its content seeds the new key's.
	Profile *nostr.Event `json:"profile,omitempty"`
	Delete  []string     `json:"delete"`
	// Kept are devPrefix app ids left as oldKey signed them.
	Kept []string `json:"kept"`
	// Dropped are oldKey app ids with no complete listing, or deleted by oldKey's kind 5.
	Dropped []string `json:"dropped"`
}

// lineageFunc returns the indexer's ancestors for an app and certificate.
type lineageFunc func(appID, cert string) ([]string, error)

// buildPlan reads oldKey's events. at is the time selection judges.
func buildPlan(old []nostr.Event, lineage lineageFunc, at time.Time) (plan, error) {
	var p plan
	kept := map[string]bool{}
	removed := map[string]int64{}
	assets := map[string]nostr.Event{}
	var listingEvents []nostr.Event
	var profile *nostr.Event

	for _, e := range old {
		switch e.Kind {
		case events.KindApp, events.KindRelease, events.KindAsset:
			appID := eventAppID(e)
			if strings.HasPrefix(appID, devPrefix) {
				kept[appID] = true
				continue
			}
			p.Delete = append(p.Delete, e.ID)
			listingEvents = append(listingEvents, e)
			if e.Kind == events.KindAsset {
				assets[e.ID] = e
			}
		case events.KindStack:
			p.Delete = append(p.Delete, e.ID)
			if strings.TrimSpace(e.Content) == "" {
				p.Stacks = append(p.Stacks, e)
			}
		case events.KindDeletion:
			p.Delete = append(p.Delete, e.ID)
			for _, coord := range events.FindAll(e.Tags, "a") {
				ref, err := events.ParseAppIdentifier(coord)
				if err != nil || ref.Pubkey != oldKey {
					continue
				}
				removed[ref.AppID] = max(removed[ref.AppID], int64(e.CreatedAt))
			}
		case events.KindAppRelays, kindFileMetadata:
			p.Delete = append(p.Delete, e.ID)
		case events.KindProfile:
			if profile == nil || e.CreatedAt > profile.CreatedAt {
				profile = &e
			}
		}
	}
	p.Profile = profile

	listed := map[string]bool{}
	for _, l := range catalog.ResolveAt(listingEvents, oldKey, at).Listings {
		listed[l.AppID] = true
		if removedAt, ok := removed[l.AppID]; ok && removedAt >= int64(l.App.CreatedAt) {
			p.Dropped = append(p.Dropped, l.AppID)
			continue
		}
		next := listing{AppID: l.AppID, App: l.App, Release: l.Release, Ancestors: map[string][]string{}}
		for _, id := range events.FindAll(l.Release.Tags, "e") {
			asset, ok := assets[id]
			if !ok {
				return plan{}, fmt.Errorf("%s: release %s names asset %s, which is not a stored 78ce… asset", l.AppID, l.Release.ID, id)
			}
			next.Assets = append(next.Assets, asset)
			cert, ok := events.Find(asset.Tags, "apk_certificate_hash")
			if !ok {
				continue
			}
			ancestors, err := lineage(l.AppID, strings.ToLower(cert))
			if err != nil {
				return plan{}, err
			}
			if len(ancestors) > 0 {
				next.Ancestors[asset.ID] = ancestors
			}
		}
		p.Listings = append(p.Listings, next)
	}
	for _, e := range listingEvents {
		if appID := eventAppID(e); e.Kind == events.KindApp && !listed[appID] && !slices.Contains(p.Dropped, appID) {
			p.Dropped = append(p.Dropped, appID)
		}
	}
	for id := range kept {
		p.Kept = append(p.Kept, id)
	}
	slices.Sort(p.Kept)
	slices.Sort(p.Dropped)
	return p, nil
}

// dropUnserved drops the listings with an asset whose file is not in Blossom.
// The relay would hold such a listing pending, not store it.
func (p *plan) dropUnserved(has func(hash string) (bool, error)) error {
	kept := p.Listings[:0]
	for _, l := range p.Listings {
		served := true
		for _, a := range l.Assets {
			x, _ := events.Find(a.Tags, "x")
			ok, err := has(x)
			if err != nil {
				return fmt.Errorf("%s: asset %s: %w", l.AppID, a.ID, err)
			}
			if !ok {
				served = false
				break
			}
		}
		if served {
			kept = append(kept, l)
		} else {
			p.Dropped = append(p.Dropped, l.AppID)
		}
	}
	p.Listings = kept
	slices.Sort(p.Dropped)
	return nil
}

// eventAppID is the app id of a listing event: d on an app, else i, else the release d before @.
func eventAppID(e nostr.Event) string {
	if e.Kind == events.KindApp {
		d, _ := events.Find(e.Tags, "d")
		return d
	}
	if i, ok := events.Find(e.Tags, "i"); ok {
		return i
	}
	d, _ := events.Find(e.Tags, "d")
	id, _, _ := strings.Cut(d, "@")
	return id
}

func appCoord(pubkey, appID string) string {
	return fmt.Sprintf("%d:%s:%s", events.KindApp, pubkey, appID)
}

// assetTags keeps the first apk_certificate_hash. Non-empty ancestors replace
// the apk_certificate_ancestor tags.
func assetTags(tags nostr.Tags, ancestors []string) nostr.Tags {
	out := make(nostr.Tags, 0, len(tags)+len(ancestors))
	seenCert := false
	for _, tag := range tags {
		if len(tag) == 0 {
			continue
		}
		switch tag[0] {
		case "apk_certificate_hash":
			if seenCert {
				continue
			}
			seenCert = true
		case "apk_certificate_ancestor":
			if len(ancestors) > 0 {
				continue
			}
		}
		out = append(out, tag)
	}
	for _, a := range ancestors {
		out = append(out, nostr.Tag{"apk_certificate_ancestor", a})
	}
	return out
}

// releaseTags sets one a tag for the new app and points every e at the new
// assets. It adds the android-arm64-v8a f tag the relay requires; selection
// already chose an arm64 asset. Older releases carry neither a nor f.
func releaseTags(tags nostr.Tags, coord string, assetIDs map[string]string) (nostr.Tags, error) {
	out := make(nostr.Tags, 0, len(tags)+2)
	out = append(out, nostr.Tag{"a", coord, relayHint})
	if !slices.Contains(events.FindAll(tags, "f"), platformARM64) {
		out = append(out, nostr.Tag{"f", platformARM64})
	}
	for _, tag := range tags {
		if len(tag) < 2 {
			out = append(out, tag)
			continue
		}
		switch tag[0] {
		case "a":
		case "e":
			id, ok := assetIDs[tag[1]]
			if !ok {
				return nil, fmt.Errorf("asset %s was not re-signed", tag[1])
			}
			out = append(out, nostr.Tag{"e", id, relayHint})
		default:
			out = append(out, tag)
		}
	}
	return out, nil
}

// appTags drops the zap tag, which the relay rejects on kind 32267.
func appTags(tags nostr.Tags) nostr.Tags {
	out := make(nostr.Tags, 0, len(tags))
	for _, tag := range tags {
		if len(tag) > 0 && tag[0] == "zap" {
			continue
		}
		out = append(out, tag)
	}
	return out
}

// stackTags moves app coordinates to the new key, except devPrefix apps, and
// adds the android-arm64-v8a f tag the relay requires.
func stackTags(tags nostr.Tags, newKey string) nostr.Tags {
	prefix := appCoord(oldKey, "")
	out := make(nostr.Tags, 0, len(tags)+1)
	if !slices.Contains(events.FindAll(tags, "f"), platformARM64) {
		out = append(out, nostr.Tag{"f", platformARM64})
	}
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == "a" && strings.HasPrefix(tag[1], prefix) {
			appID := strings.TrimPrefix(tag[1], prefix)
			if !strings.HasPrefix(appID, devPrefix) {
				out = append(out, nostr.Tag{"a", appCoord(newKey, appID), relayHint})
				continue
			}
		}
		out = append(out, tag)
	}
	return out
}

// profileContent is content with lud16 set.
func profileContent(content, lud16 string) (string, error) {
	fields := map[string]json.RawMessage{}
	if strings.TrimSpace(content) != "" {
		if err := json.Unmarshal([]byte(content), &fields); err != nil {
			return "", fmt.Errorf("kind 0 content: %w", err)
		}
	}
	raw, err := json.Marshal(lud16)
	if err != nil {
		return "", err
	}
	fields["lud16"] = raw
	out, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// signFunc sets the pubkey, the id, and the signature.
type signFunc func(context.Context, *nostr.Event) error

// resign returns the new events in publish order: each listing's assets,
// release, and app, then the stacks, the kind 0, and the kind 10067.
// One listing at a time keeps the relay's buffer zone small, so each app is
// promoted as it arrives.
func resign(ctx context.Context, p plan, sign signFunc, newKey, lud16 string, now time.Time) ([]nostr.Event, error) {
	var out []nostr.Event
	add := func(e nostr.Event) (string, error) {
		if err := sign(ctx, &e); err != nil {
			return "", fmt.Errorf("sign kind %d: %w", e.Kind, err)
		}
		if e.PubKey != newKey {
			return "", fmt.Errorf("signed as %s, want %s", e.PubKey, newKey)
		}
		out = append(out, e)
		return e.ID, nil
	}

	for _, l := range p.Listings {
		assetIDs := map[string]string{}
		for _, a := range l.Assets {
			id, err := add(nostr.Event{CreatedAt: a.CreatedAt, Kind: a.Kind, Tags: assetTags(a.Tags, l.Ancestors[a.ID]), Content: a.Content})
			if err != nil {
				return nil, err
			}
			assetIDs[a.ID] = id
		}
		tags, err := releaseTags(l.Release.Tags, appCoord(newKey, l.AppID), assetIDs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", l.AppID, err)
		}
		if _, err := add(nostr.Event{CreatedAt: l.Release.CreatedAt, Kind: l.Release.Kind, Tags: tags, Content: l.Release.Content}); err != nil {
			return nil, err
		}
		if _, err := add(nostr.Event{CreatedAt: l.App.CreatedAt, Kind: l.App.Kind, Tags: appTags(l.App.Tags), Content: l.App.Content}); err != nil {
			return nil, err
		}
	}
	for _, s := range p.Stacks {
		if _, err := add(nostr.Event{CreatedAt: s.CreatedAt, Kind: s.Kind, Tags: stackTags(s.Tags, newKey), Content: s.Content}); err != nil {
			return nil, err
		}
	}
	base := ""
	if p.Profile != nil {
		base = p.Profile.Content
	}
	content, err := profileContent(base, lud16)
	if err != nil {
		return nil, err
	}
	stamp := nostr.Timestamp(now.Unix())
	if _, err := add(nostr.Event{CreatedAt: stamp, Kind: events.KindProfile, Tags: nostr.Tags{}, Content: content}); err != nil {
		return nil, err
	}
	if _, err := add(nostr.Event{CreatedAt: stamp, Kind: events.KindAppRelays, Tags: nostr.Tags{{"r", relayHint}}}); err != nil {
		return nil, err
	}
	return out, nil
}

func (p plan) appIDs() []string {
	ids := make([]string, len(p.Listings))
	for i, l := range p.Listings {
		ids[i] = l.AppID
	}
	return ids
}

// lud16Of is the lud16 in a kind 0, or empty.
func lud16Of(profile *nostr.Event) string {
	if profile == nil {
		return ""
	}
	var body struct {
		Lud16 string `json:"lud16"`
	}
	_ = json.Unmarshal([]byte(profile.Content), &body) // a bad profile has no lud16
	return body.Lud16
}
