package blossom

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pippellia-btc/blossom"
	"github.com/zapstore/relay/pkg/events"
)

const apkType = "application/vnd.android.package-archive"

// APKHashes returns the file hashes named by current asset events.
// assets is how many kind 3063 rows were read. Zero means that table is empty.
func APKHashes(ctx context.Context, db *sql.DB) (keep map[string]struct{}, assets int, err error) {
	rows, err := db.QueryContext(ctx, `SELECT tags FROM events WHERE kind = ?`, events.KindAsset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	keep = map[string]struct{}{}
	for rows.Next() {
		assets++
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, assets, err
		}
		var tags nostr.Tags
		if err := json.Unmarshal([]byte(raw), &tags); err != nil {
			continue
		}
		if hash, ok := events.Find(tags, "x"); ok && hash != "" {
			keep[hash] = struct{}{}
		}
	}
	return keep, assets, rows.Err()
}

// PruneAPKs deletes APK blobs that no current asset event names.
// keep is the set of hashes to retain. Bunny and the local blob directory are both cleared.
func (b *T) PruneAPKs(ctx context.Context, keep map[string]struct{}) (int, error) {
	rows, err := b.store.DB.QueryContext(ctx, `SELECT hash, type FROM blobs WHERE type = ?`, apkType)
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
			slog.Error("prune: remove apk", "err", err, "hash", hash)
			continue
		}
		if err := b.store.Delete(ctx, parsed); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}
