package blossom

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"regexp"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/blossom/store"
)

var hashInText = regexp.MustCompile(`[0-9a-f]{64}`)

// ReferencedHashes returns file hashes named by current events and by pending events.
// n is how many event rows were read, including pending. Zero means both tables are empty.
func ReferencedHashes(ctx context.Context, db *sql.DB) (keep map[string]struct{}, n int, err error) {
	keep = map[string]struct{}{}
	rows, err := db.QueryContext(ctx, `SELECT tags FROM events`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		n++
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, n, err
		}
		collectHashes(keep, raw)
	}
	if err := rows.Err(); err != nil {
		return nil, n, err
	}
	pending, err := db.QueryContext(ctx, `SELECT raw FROM pending_events`)
	if err != nil {
		return nil, n, err
	}
	defer pending.Close()
	for pending.Next() {
		n++
		var raw string
		if err := pending.Scan(&raw); err != nil {
			return nil, n, err
		}
		collectHashes(keep, raw)
	}
	return keep, n, pending.Err()
}

func collectHashes(keep map[string]struct{}, raw string) {
	var tags nostr.Tags
	if err := json.Unmarshal([]byte(raw), &tags); err == nil {
		for _, tag := range tags {
			for _, value := range tag {
				for _, hash := range hashInText.FindAllString(value, -1) {
					keep[hash] = struct{}{}
				}
			}
		}
		return
	}
	for _, hash := range hashInText.FindAllString(raw, -1) {
		keep[hash] = struct{}{}
	}
}

// Prune deletes served blobs that no current or pending event names.
// keep is the set of hashes to retain. A blob in the buffer zone is left to expiry.
func (b *T) Prune(ctx context.Context, keep map[string]struct{}) (int, error) {
	served, err := b.store.InState(ctx, store.Served, time.Time{})
	if err != nil {
		return 0, err
	}
	var n int
	for _, meta := range served {
		if _, ok := keep[meta.Hash.Hex()]; ok {
			continue
		}
		if err := b.blobs.remove(ctx, meta); err != nil {
			slog.Error("prune: remove blob", "err", err, "hash", meta.Hash)
			continue
		}
		n++
	}
	return n, nil
}
