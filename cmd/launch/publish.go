package main

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/policy"
	"github.com/zapstore/relay/pkg/relay/store"
)

// report counts what was stored.
type report struct {
	Stored int
	// Banned are app ids with a listing event the relay's policy rejects.
	Banned map[string]bool
}

// storeAll writes the re-signed events into relay.db while the relay is
// stopped. Every listing was already listed under oldKey, so the relay's
// admission gate has nothing left to check, except a ban: a listing with an
// event the policy rejects is skipped whole.
func storeAll(ctx context.Context, t term, relayData, catalogKey string, evs []nostr.Event) (report, error) {
	rep := report{Banned: map[string]bool{}}
	db, err := store.New(filepath.Join(relayData, "relay.db"))
	if err != nil {
		return rep, err
	}
	defer db.Close()
	pol, err := policy.Open(db.DB)
	if err != nil {
		return rep, err
	}
	for i := range evs {
		if !listingEvent(evs[i]) {
			continue
		}
		verdict, err := pol.EventVerdict(ctx, &evs[i])
		if err != nil {
			return rep, err
		}
		if verdict.Reject || verdict.Reason != "" {
			rep.Banned[eventAppID(evs[i])] = true
		}
	}

	t.say("Storing %d events in %s", len(evs), filepath.Join(relayData, "relay.db"))
	for i, e := range evs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if skipEvent(e, rep.Banned) {
			continue
		}
		if nostr.IsReplaceableKind(e.Kind) || nostr.IsAddressableKind(e.Kind) {
			_, err = db.Replace(ctx, &e)
		} else {
			_, err = db.Save(ctx, &e)
		}
		if err != nil {
			return rep, fmt.Errorf("store kind %d %s (%s): %w", e.Kind, e.ID, eventAppID(e), err)
		}
		rep.Stored++
		if (i+1)%500 == 0 {
			t.say("  %d of %d", i+1, len(evs))
		}
	}
	return rep, nil
}

func listingEvent(e nostr.Event) bool {
	switch e.Kind {
	case events.KindApp, events.KindRelease, events.KindAsset:
		return true
	default:
		return false
	}
}

// skipEvent is true for every event of a banned listing.
func skipEvent(e nostr.Event, banned map[string]bool) bool {
	return listingEvent(e) && banned[eventAppID(e)]
}

func (r report) print(t term) {
	t.say("Stored %d events.", r.Stored)
	if len(r.Banned) > 0 {
		t.say("Skipped %d banned apps: %v", len(r.Banned), slices.Sorted(maps.Keys(r.Banned)))
	}
}

// storedApps returns which app ids have a kind 32267 from pubkey.
func storedApps(ctx context.Context, sock *nostrSocket, pubkey string, ids []string) (map[string]bool, error) {
	stored := map[string]bool{}
	for chunk := range slices.Chunk(ids, 100) {
		evs, err := sock.query(ctx, nostr.Filter{
			Kinds:   []int{events.KindApp},
			Authors: []string{pubkey},
			Tags:    nostr.TagMap{"d": chunk},
			Limit:   len(chunk) * 2,
		})
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			if d, ok := events.Find(e.Tags, "d"); ok {
				stored[d] = true
			}
		}
	}
	return stored, nil
}

// checkRotation reports what the new key's relay serves. It returns an error when any planned app is missing.
func checkRotation(ctx context.Context, t term, sock *nostrSocket, newKey string, p plan, rep report) error {
	stored, err := storedApps(ctx, sock, newKey, p.appIDs())
	if err != nil {
		return err
	}
	var missing []string
	for _, id := range p.appIDs() {
		if !stored[id] && !rep.Banned[id] {
			missing = append(missing, id)
		}
	}
	t.say("%d of %d apps are stored under the new key.", len(stored), len(p.Listings))

	kept, err := storedApps(ctx, sock, oldKey, p.Kept)
	if err != nil {
		return err
	}
	for _, id := range p.Kept {
		if !kept[id] {
			t.say("  ! %s is no longer served under 78ce…", id)
			missing = append(missing, id)
		}
	}
	relays, err := sock.query(ctx, nostr.Filter{Kinds: []int{events.KindAppRelays}, Authors: []string{newKey}, Limit: 1})
	if err != nil {
		return err
	}
	if len(relays) == 0 {
		t.say("  ! the new key's kind 10067 is not stored")
		missing = append(missing, "kind 10067")
	}
	if len(missing) > 0 {
		shown := missing[:min(len(missing), 30)]
		return fmt.Errorf("%d missing: %v", len(missing), shown)
	}
	return nil
}
