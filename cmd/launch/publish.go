package main

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay"
)

// msgBlobPending must match relay/pkg/relay/listing.go: the asset's file is not in Blossom.
const msgBlobPending = "the event will be saved when the referenced blob is uploaded"

// report counts what the relay answered.
type report struct {
	Published int
	// Banned are app ids whose asset names a banned file hash.
	Banned map[string]bool
	// Failed counts every other rejection by reason.
	Failed  map[string]int
	Samples []string
}

func (r report) failures() int {
	n := 0
	for _, c := range r.Failed {
		n += c
	}
	return n
}

// publishAll publishes in order. keepGoing collects failures instead of stopping at the first.
func publishAll(ctx context.Context, t term, sock *nostrSocket, evs []nostr.Event, keepGoing bool) (report, error) {
	rep := report{Banned: map[string]bool{}, Failed: map[string]int{}}
	t.say("Publishing %d events over %s", len(evs), sock.path)
	for i, e := range evs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		ok, msg, err := sock.publish(ctx, e)
		if err != nil {
			return rep, err
		}
		switch {
		case !ok && msg == relay.ErrFileNotAllowed.Error():
			rep.Banned[eventAppID(e)] = true
			continue
		case ok && msg == msgBlobPending:
			ok, msg = false, "asset file is not in Blossom"
		}
		if !ok {
			rep.Failed[msg]++
			sample := fmt.Sprintf("kind %d %s (%s): %s", e.Kind, e.ID, eventAppID(e), msg)
			if len(rep.Samples) < 20 {
				rep.Samples = append(rep.Samples, sample)
			}
			if !keepGoing {
				return rep, fmt.Errorf("relay rejected %s", sample)
			}
			continue
		}
		rep.Published++
		if (i+1)%500 == 0 {
			t.say("  %d of %d", i+1, len(evs))
		}
	}
	return rep, nil
}

func (r report) print(t term) {
	t.say("Published %d events.", r.Published)
	if len(r.Banned) > 0 {
		t.say("Skipped %d apps with a banned file hash: %v", len(r.Banned), slices.Sorted(maps.Keys(r.Banned)))
	}
	for _, reason := range slices.Sorted(maps.Keys(r.Failed)) {
		t.say("  ! %d rejected: %s", r.Failed[reason], reason)
	}
	for _, s := range r.Samples {
		t.say("    %s", s)
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
