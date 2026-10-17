package catalog

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

const mainChannel = "main"

// State is the canonical catalog at one snapshot.
type State struct {
	Listings []Listing
	Proofs   []Proof
	Stacks   []Stack
	Profiles []Profile
	Relays   []Proof
	// Notices is operator text for removed apps. It is not part of the event set.
	Notices map[string]string
}

// Profile is the current kind 0 event for one pubkey.
type Profile struct {
	Pubkey  string
	Picture string
	Event   nostr.Event
}

// Listing is a complete main-channel Android relation signed by one pubkey.
type Listing struct {
	AppID   string
	App     nostr.Event
	Release nostr.Event
	Assets  []nostr.Event
}

// Proof is the current kind 30509 event for one coordinate.
type Proof struct {
	Pubkey string
	D      string
	Event  nostr.Event
}

// Stack is the current kind 30267 event signed by the curator.
type Stack struct {
	Pubkey string
	D      string
	Event  nostr.Event
}

// listingIdentity is the event ids that make up one listing.
type listingIdentity struct {
	AppEventID     string
	ReleaseEventID string
	AssetEventIDs  []string
}

// Events is the current listing triples and replaceable events, without historical releases.
func (s State) Events() []nostr.Event {
	var out []nostr.Event
	for _, listing := range s.Listings {
		out = append(out, listingEvents(listing)...)
	}
	return append(out, s.replaceables()...)
}

// replaceables is every event in the state that is not part of a listing.
func (s State) replaceables() []nostr.Event {
	var out []nostr.Event
	for _, proof := range s.Proofs {
		out = append(out, proof.Event)
	}
	for _, stack := range s.Stacks {
		out = append(out, stack.Event)
	}
	for _, profile := range s.Profiles {
		out = append(out, profile.Event)
	}
	for _, relay := range s.Relays {
		out = append(out, relay.Event)
	}
	return out
}

// Resolve builds canonical version-1 catalog state from a consistent event snapshot.
// stackPubkey selects kind 30267 stacks. An empty stackPubkey keeps every stack in the
// input. The live relay passes the catalog key. A snapshot file passes empty: it already
// holds the stacks that epoch shipped, including ones signed before the catalog key.
// Resolve builds state at the current time.
func Resolve(snapshot []nostr.Event, stackPubkey string) State {
	return ResolveAt(snapshot, stackPubkey, time.Now())
}

func ResolveAt(snapshot []nostr.Event, stackPubkey string, at time.Time) State {
	return resolveAt(snapshot, stackPubkey, stackPubkey, at)
}

func resolveAt(snapshot []nostr.Event, stackPubkey, catalogKey string, at time.Time) State {
	current := map[string]nostr.Event{}
	assets := map[string]nostr.Event{}

	for _, event := range snapshot {
		switch event.Kind {
		case events.KindApp, events.KindRelease, events.KindIdentityProof, events.KindStack, events.KindProfile, events.KindAppRelays:
			if event.Kind == events.KindStack && stackPubkey != "" && event.PubKey != stackPubkey {
				continue
			}
			d, ok := events.Find(event.Tags, "d")
			if event.Kind == events.KindProfile || event.Kind == events.KindAppRelays {
				d, ok = "", event.PubKey != ""
			}
			if !ok {
				continue
			}
			key := fmt.Sprintf("%d:%s:%s", event.Kind, event.PubKey, d)
			if prev, ok := current[key]; ok && !betterEvent(event, prev) {
				continue
			}
			current[key] = event
		case events.KindAsset:
			assets[event.ID] = event
		}
	}

	// Releases grouped by signer and app id. Production listings join on `i`;
	// `a` is optional and checked in selectListing when present.
	releases := map[string][]nostr.Event{}
	proofs := make([]Proof, 0, len(current))
	stacks := make([]Stack, 0)
	var apps []nostr.Event
	var profiles []Profile
	var relays []Proof
	for _, event := range current {
		switch event.Kind {
		case events.KindApp:
			app, err := events.ParseApp(&event)
			if err != nil || app.Validate() != nil {
				continue
			}
			apps = append(apps, event)
		case events.KindRelease:
			if appID, ok := events.Find(event.Tags, "i"); ok {
				key := ownerKey(event.PubKey, appID)
				releases[key] = append(releases[key], event)
			}
		case events.KindIdentityProof:
			d, _ := events.Find(event.Tags, "d")
			proofs = append(proofs, Proof{Pubkey: event.PubKey, D: d, Event: event})
		case events.KindStack:
			parsed, err := events.ParseStack(&event)
			if err != nil || parsed.Validate() != nil {
				continue
			}
			d, _ := events.Find(event.Tags, "d")
			stacks = append(stacks, Stack{Pubkey: event.PubKey, D: d, Event: event})
		case events.KindProfile:
			profiles = append(profiles, Profile{
				Pubkey:  event.PubKey,
				Picture: pictureURL(event),
				Event:   event,
			})
		case events.KindAppRelays:
			relays = append(relays, Proof{Pubkey: event.PubKey, Event: event})
		}
	}

	candidates := map[string][]Listing{}
	for _, app := range apps {
		appID, _ := events.Find(app.Tags, "d")
		listing, ok := selectListing(appID, app, releases[ownerKey(app.PubKey, appID)], assets)
		if !ok {
			continue
		}
		candidates[appID] = append(candidates[appID], listing)
	}
	var listings []Listing
	for _, group := range candidates {
		chosen, ok := chooseListing(group, proofs, at, catalogKey)
		if !ok {
			continue
		}
		listings = append(listings, chosen)
	}
	slices.SortFunc(listings, func(a, b Listing) int {
		return strings.Compare(a.AppID, b.AppID)
	})
	slices.SortFunc(proofs, func(a, b Proof) int {
		if c := strings.Compare(a.Pubkey, b.Pubkey); c != 0 {
			return c
		}
		return strings.Compare(a.D, b.D)
	})
	slices.SortFunc(stacks, func(a, b Stack) int {
		if c := strings.Compare(a.Pubkey, b.Pubkey); c != 0 {
			return c
		}
		return strings.Compare(a.D, b.D)
	})
	slices.SortFunc(profiles, func(a, b Profile) int {
		return strings.Compare(a.Pubkey, b.Pubkey)
	})
	slices.SortFunc(relays, func(a, b Proof) int {
		return strings.Compare(a.Pubkey, b.Pubkey)
	})
	return State{Listings: listings, Proofs: proofs, Stacks: stacks, Profiles: profiles, Relays: relays}
}

func pictureURL(event nostr.Event) string {
	var body struct {
		Picture string `json:"picture"`
	}
	if json.Valid([]byte(event.Content)) {
		_ = json.Unmarshal([]byte(event.Content), &body)
	}
	return strings.TrimSpace(body.Picture)
}

type listingCandidate struct {
	release     nostr.Event
	assets      []nostr.Event
	versionCode int64
}

func ownerKey(pubkey, appID string) string {
	return pubkey + "\x00" + appID
}

const (
	androidAPKMIME = "application/vnd.android.package-archive"
	preferredABI   = "android-arm64-v8a"
)

// isAndroidAPK reports whether the asset includes android-arm64-v8a.
// Other platforms may be present. A MIME type, when present, must be an Android package.
func isAndroidAPK(asset events.Asset) bool {
	if asset.MimeType != "" && asset.MimeType != androidAPKMIME {
		return false
	}
	return slices.Contains(asset.Platforms, preferredABI)
}

// selectListing picks the best main-channel android-arm64-v8a release among releases,
// which must all be signed by app.PubKey and name appID in their `i` tag.
func selectListing(appID string, app nostr.Event, releases []nostr.Event, assets map[string]nostr.Event) (Listing, bool) {
	var chosen *listingCandidate
	for _, releaseEvent := range releases {
		release, err := events.ParseRelease(&releaseEvent)
		if err != nil {
			continue
		}
		if release.I != appID || release.Channel != mainChannel || release.Version == "" ||
			release.D != release.I+"@"+release.Version || len(release.AssetIDs) == 0 {
			continue
		}
		if a := releaseEvent.Tags.GetFirst([]string{"a"}); a != nil && len(*a) > 1 {
			ref, err := events.ParseAppIdentifier((*a)[1])
			if err != nil || ref.AppID != appID || ref.Pubkey != app.PubKey {
				continue
			}
		}
		type androidAsset struct {
			event nostr.Event
			code  int64
		}
		var selected []androidAsset
		var maxCode int64 = -1
		for _, assetID := range release.AssetIDs {
			assetEvent, ok := assets[assetID]
			if !ok || assetEvent.PubKey != app.PubKey {
				continue
			}
			asset, err := events.ParseAsset(&assetEvent)
			if err != nil || asset.Validate() != nil {
				continue
			}
			if asset.I != appID || !isAndroidAPK(asset) {
				continue
			}
			code, err := strconv.ParseInt(asset.VersionCode, 10, 64)
			if err != nil {
				continue
			}
			selected = append(selected, androidAsset{event: assetEvent, code: code})
			if code > maxCode {
				maxCode = code
			}
		}
		if len(selected) == 0 || maxCode < 0 {
			continue
		}
		winning := make([]nostr.Event, 0, len(selected))
		for _, asset := range selected {
			if asset.code == maxCode {
				winning = append(winning, asset.event)
			}
		}
		if len(winning) == 0 {
			continue
		}
		slices.SortFunc(winning, func(a, b nostr.Event) int {
			return strings.Compare(a.ID, b.ID)
		})
		next := listingCandidate{release: releaseEvent, assets: winning, versionCode: maxCode}
		if chosen == nil || betterCandidate(next, *chosen) {
			c := next
			chosen = &c
		}
	}
	if chosen == nil {
		return Listing{}, false
	}
	return Listing{
		AppID:   appID,
		App:     app,
		Release: chosen.release,
		Assets:  chosen.assets,
	}, true
}

func chooseListing(group []Listing, proofs []Proof, at time.Time, catalogKey string) (Listing, bool) {
	appCert := ""
	if catalogKey != "" {
		for _, listing := range group {
			if listing.App.PubKey == catalogKey {
				if cert, ok := listingCertificate(listing); ok {
					appCert = cert
				}
			}
		}
	}
	var eligible []Listing
	for _, listing := range group {
		if catalogKey != "" && listing.App.PubKey != catalogKey && !isDeveloperSlot(listing, proofs, at, appCert) {
			continue
		}
		eligible = append(eligible, listing)
	}
	if len(eligible) == 0 {
		return Listing{}, false
	}
	chosen := eligible[0]
	for _, listing := range eligible[1:] {
		if betterListing(listing, chosen, proofs, at, catalogKey, appCert) {
			chosen = listing
		}
	}
	return chosen, true
}

func betterListing(next, current Listing, proofs []Proof, at time.Time, catalogKey, appCert string) bool {
	nv := listingVersionCode(next)
	cv := listingVersionCode(current)
	if nv != cv {
		return nv > cv
	}
	nextDev := isDeveloperSlot(next, proofs, at, appCertFor(next, current, catalogKey, appCert))
	currentDev := isDeveloperSlot(current, proofs, at, appCertFor(next, current, catalogKey, appCert))
	if nextDev != currentDev {
		return nextDev
	}
	return betterEvent(next.App, current.App)
}

func appCertFor(next, current Listing, catalogKey, appCert string) string {
	if appCert != "" {
		return appCert
	}
	if catalogKey == "" {
		return ""
	}
	if next.App.PubKey == catalogKey {
		if cert, ok := listingCertificate(next); ok {
			return cert
		}
	}
	if current.App.PubKey == catalogKey {
		if cert, ok := listingCertificate(current); ok {
			return cert
		}
	}
	return ""
}

func listingVersionCode(listing Listing) int64 {
	var max int64 = -1
	for _, asset := range listing.Assets {
		parsed, err := events.ParseAsset(&asset)
		if err != nil {
			continue
		}
		code, err := strconv.ParseInt(parsed.VersionCode, 10, 64)
		if err != nil {
			continue
		}
		if code > max {
			max = code
		}
	}
	return max
}

func isDeveloperSlot(listing Listing, proofs []Proof, at time.Time, appCert string) bool {
	signer, ancestors, ok := listingChain(listing)
	if !ok {
		return false
	}
	cert := signer
	if appCert != "" {
		appCert = strings.ToLower(appCert)
		if signer != appCert && !slices.Contains(ancestors, appCert) {
			return false
		}
		cert = appCert
	}
	owner, delegate, ok := c1Publisher(proofs, []string{cert}, at)
	if !ok {
		return false
	}
	return listing.App.PubKey == owner || (delegate != "" && listing.App.PubKey == delegate)
}

func listingCertificate(listing Listing) (string, bool) {
	signer, _, ok := listingChain(listing)
	return signer, ok
}

// listingChain is an asset's signer and its ancestors. Extra apk_certificate_hash
// values are ignored.
func listingChain(listing Listing) (signer string, ancestors []string, ok bool) {
	for _, asset := range listing.Assets {
		hash, found := events.Find(asset.Tags, "apk_certificate_hash")
		if !found || hash == "" {
			continue
		}
		older := events.FindAll(asset.Tags, "apk_certificate_ancestor")
		for i := range older {
			older[i] = strings.ToLower(older[i])
		}
		return strings.ToLower(hash), older, true
	}
	return "", nil, false
}

// c1Publisher selects the active C1 owner for the certificates in certs.
// Callers pass the app's current signer. Ancestor proofs do not count.
// Replacement first: one current event per pubkey, greatest created_at, then
// lowest event id. A revoked or expired current event drops that pubkey.
// The owner is the newest pubkey that remains.
func c1Publisher(proofs []Proof, certs []string, at time.Time) (owner, delegate string, ok bool) {
	want := map[string]struct{}{}
	for _, cert := range certs {
		want[strings.ToLower(cert)] = struct{}{}
	}
	current := map[string]Proof{}
	for i := range proofs {
		if _, ok := want[strings.ToLower(proofs[i].D)]; !ok {
			continue
		}
		key := proofs[i].Event.PubKey
		prev, exists := current[key]
		if exists && !betterEvent(proofs[i].Event, prev.Event) {
			continue
		}
		current[key] = proofs[i]
	}
	var best *Proof
	for _, proof := range current {
		if !events.ProofActive(proof.Event, at) {
			continue
		}
		if best == nil || betterEvent(proof.Event, best.Event) {
			chosen := proof
			best = &chosen
		}
	}
	if best == nil {
		return "", "", false
	}
	delegate, _ = events.Find(best.Event.Tags, "delegation")
	return best.Pubkey, delegate, true
}

func betterEvent(next, current nostr.Event) bool {
	if next.CreatedAt != current.CreatedAt {
		return next.CreatedAt > current.CreatedAt
	}
	return next.ID < current.ID
}

func betterCandidate(next, current listingCandidate) bool {
	if next.versionCode != current.versionCode {
		return next.versionCode > current.versionCode
	}
	if next.release.CreatedAt != current.release.CreatedAt {
		return next.release.CreatedAt > current.release.CreatedAt
	}
	return next.release.ID < current.release.ID
}

func listingIndex(listing Listing) listingIdentity {
	ids := make([]string, len(listing.Assets))
	for i, asset := range listing.Assets {
		ids[i] = asset.ID
	}
	slices.Sort(ids)
	return listingIdentity{
		AppEventID:     listing.App.ID,
		ReleaseEventID: listing.Release.ID,
		AssetEventIDs:  ids,
	}
}

func listingEvents(listing Listing) []nostr.Event {
	out := make([]nostr.Event, 0, 2+len(listing.Assets))
	out = append(out, listing.App, listing.Release)
	out = append(out, listing.Assets...)
	return out
}
