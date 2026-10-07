package blossom

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"regexp"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/blossom"
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

// Prune deletes blobs that no current or pending event names.
// keep is the set of hashes to retain. Bunny and the local blob directory are both cleared.
func (b *T) Prune(ctx context.Context, keep map[string]struct{}) (int, error) {
	rows, err := b.store.DB.QueryContext(ctx, `SELECT hash, type FROM blobs`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var hash, mime string
		if err := rows.Scan(&hash, &mime); err != nil {
			return n, err
		}
		if _, ok := keep[hash]; ok {
			continue
		}
		parsed, err := blossom.ParseHash(hash)
		if err != nil {
			continue
		}
		name := BlobPath(parsed, mime)
		if err := b.removeBlob(ctx, name); err != nil {
			slog.Error("prune: remove blob", "err", err, "hash", hash)
			continue
		}
		if err := b.store.Delete(ctx, parsed); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}
