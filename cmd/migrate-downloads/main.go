package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/relay/store"
)

var ctx = context.Background()

func main() {
	analyticsDB := flag.String("analytics-db", "", "path to analytics sqlite db")
	relayDB := flag.String("relay-db", "", "path to relay/nostr sqlite db")
	flag.Parse()

	if *analyticsDB == "" {
		fmt.Println("analytics-db is required")
		return
	}
	if *relayDB == "" {
		fmt.Println("relay-db is required")
		return
	}

	// 1. Open both DBs
	adb, err := sql.Open("sqlite3", *analyticsDB)
	if err != nil {
		panic(err)
	}
	defer adb.Close()

	rdb, err := store.New(*relayDB)
	if err != nil {
		panic(err)
	}
	defer rdb.Close()

	// 2. Add the new columns and indexes if not already there (idempotent)
	for _, stmt := range []string{
		`ALTER TABLE downloads ADD COLUMN app_id      TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE downloads ADD COLUMN app_version TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE downloads ADD COLUMN app_pubkey  TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE downloads ADD COLUMN type        TEXT NOT NULL DEFAULT 'unknown'`,
	} {
		if _, err := adb.Exec(stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				panic(err)
			}
		}
	}
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS downloads_type      ON downloads (type)`,
		`CREATE INDEX IF NOT EXISTS downloads_app_id    ON downloads (app_id)`,
		`CREATE INDEX IF NOT EXISTS downloads_app_pubkey ON downloads (app_pubkey)`,
	} {
		if _, err := adb.Exec(stmt); err != nil {
			panic(err)
		}
	}

	// 3. Fetch all distinct hashes from downloads
	rows, err := adb.QueryContext(ctx, `SELECT DISTINCT hash FROM downloads`)
	if err != nil {
		panic(err)
	}
	defer rows.Close()

	updated, deleted, skipped := 0, 0, 0
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			panic(err)
		}

		// 4. Find the kind 3063 asset event referencing this hash via the "x" tag
		filter := nostr.Filter{
			Kinds: []int{events.KindAsset},
			Tags:  nostr.TagMap{"x": []string{hash}},
			Limit: 1,
		}

		assets, err := rdb.Query(ctx, filter)
		if err != nil {
			panic(err)
		}
		if len(assets) == 0 {
			if _, err = adb.ExecContext(ctx, "DELETE FROM downloads WHERE hash = ?", hash); err != nil {
				panic(err)
			}
			deleted++
			continue
		}
		if len(assets) > 1 {
			slog.Info("multiple assets per blob", "hash", hash, "assets", len(assets))
			for i, asset := range assets {
				slog.Info(fmt.Sprintf("%d) %s", i, asset.ID))
			}
			return
		}

		asset := assets[0]
		appID, ok := events.Find(asset.Tags, "i")
		if !ok {
			skipped++
			slog.Info("no app id found in asset", "hash", hash, "asset", asset.ID)
			continue
		}

		appVersion, ok := events.Find(asset.Tags, "version")
		if !ok {
			skipped++
			slog.Info("no app version found in asset", "hash", hash, "asset", asset.ID)
			continue
		}

		// 5. Backfill all rows for this hash
		if _, err = adb.ExecContext(ctx,
			`UPDATE downloads SET app_id = ?, app_version = ?, app_pubkey = ? WHERE hash = ?`,
			appID, appVersion, asset.PubKey, hash,
		); err != nil {
			panic(err)
		}
		updated++
	}

	fmt.Printf("done: updated=%d deleted=%d skipped=%d\n", updated, deleted, skipped)
}
