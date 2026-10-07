package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

// Diff is the change from one resolved catalog to the next.
type Diff struct {
	Events     []nostr.Event
	AppDeletes []string
	Coords     []Coord
	Apps       []string // app ids whose artifact directory is copied
	Avatars    []string // <hex pubkey>.webp filenames, without a directory
	Notices    map[string]string
}

type Coord struct {
	Kind   int
	Pubkey string
	D      string
}

func Compare(prev, next State) Diff {
	prevEvents := map[string]nostr.Event{}
	for _, event := range prev.Events() {
		prevEvents[event.ID] = event
	}
	var d Diff
	for _, event := range next.Events() {
		if _, ok := prevEvents[event.ID]; !ok {
			d.Events = append(d.Events, event)
		}
	}
	slices.SortFunc(d.Events, func(a, b nostr.Event) int {
		return strings.Compare(a.ID, b.ID)
	})

	prevApps := map[string]struct{}{}
	for _, listing := range prev.Listings {
		prevApps[listing.AppID] = struct{}{}
	}
	nextApps := map[string]IndexListing{}
	for _, listing := range next.Index().Listings {
		nextApps[listing.AppID] = listing
	}
	prevIdx := map[string]IndexListing{}
	for _, listing := range prev.Index().Listings {
		prevIdx[listing.AppID] = listing
	}
	for id := range prevApps {
		if _, ok := nextApps[id]; !ok {
			d.AppDeletes = append(d.AppDeletes, id)
		}
	}
	slices.Sort(d.AppDeletes)
	for id, cur := range nextApps {
		old, ok := prevIdx[id]
		if !ok || old.AppEventID != cur.AppEventID || old.ReleaseEventID != cur.ReleaseEventID || !sameIDs(old.AssetEventIDs, cur.AssetEventIDs) {
			d.Apps = append(d.Apps, id)
		}
	}
	slices.Sort(d.Apps)
	d.Coords = missingCoords(prev, next)
	return d
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func missingCoords(prev, next State) []Coord {
	nextKeys := map[string]struct{}{}
	for _, proof := range next.Proofs {
		nextKeys[coordKey(events.KindIdentityProof, proof.Pubkey, proof.D)] = struct{}{}
	}
	for _, stack := range next.Stacks {
		nextKeys[coordKey(events.KindStack, stack.Pubkey, stack.D)] = struct{}{}
	}
	var out []Coord
	for _, proof := range prev.Proofs {
		key := coordKey(events.KindIdentityProof, proof.Pubkey, proof.D)
		if _, ok := nextKeys[key]; !ok {
			out = append(out, Coord{Kind: events.KindIdentityProof, Pubkey: proof.Pubkey, D: proof.D})
		}
	}
	for _, stack := range prev.Stacks {
		key := coordKey(events.KindStack, stack.Pubkey, stack.D)
		if _, ok := nextKeys[key]; !ok {
			out = append(out, Coord{Kind: events.KindStack, Pubkey: stack.Pubkey, D: stack.D})
		}
	}
	for _, relay := range next.Relays {
		nextKeys[coordKey(events.KindAppRelays, relay.Pubkey, "")] = struct{}{}
	}
	for _, relay := range prev.Relays {
		key := coordKey(events.KindAppRelays, relay.Pubkey, "")
		if _, ok := nextKeys[key]; !ok {
			out = append(out, Coord{Kind: events.KindAppRelays, Pubkey: relay.Pubkey, D: ""})
		}
	}
	slices.SortFunc(out, func(a, b Coord) int {
		if c := a.Kind - b.Kind; c != 0 {
			return c
		}
		if c := strings.Compare(a.Pubkey, b.Pubkey); c != 0 {
			return c
		}
		return strings.Compare(a.D, b.D)
	})
	return out
}

func coordKey(kind int, pubkey, d string) string {
	return fmt.Sprintf("%d:%s:%s", kind, pubkey, d)
}

// MarshalDiff writes event lines, coordinate deletes, then one app-id delete line.
func MarshalDiff(d Diff) ([]byte, error) {
	var buf bytes.Buffer
	for _, event := range d.Events {
		raw, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	for _, coord := range d.Coords {
		raw, err := json.Marshal(struct {
			Kind   int    `json:"kind"`
			Pubkey string `json:"pubkey"`
			D      string `json:"d"`
		}{coord.Kind, coord.Pubkey, coord.D})
		if err != nil {
			return nil, err
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	if len(d.AppDeletes) > 0 {
		raw, err := json.Marshal(struct {
			Delete []string `json:"delete"`
		}{d.AppDeletes})
		if err != nil {
			return nil, err
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	for _, appID := range d.AppDeletes {
		reason := strings.TrimSpace(d.Notices[appID])
		if reason == "" {
			continue
		}
		raw, err := json.Marshal(struct {
			Banned bool   `json:"banned"`
			AppID  string `json:"app_id"`
			Reason string `json:"reason"`
		}{true, appID, reason})
		if err != nil {
			return nil, err
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}
