package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"time"

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

	// 2. Add the new column if not already there (idempotent)
	adb.Exec(`ALTER TABLE impressions ADD COLUMN app_version TEXT NOT NULL DEFAULT ''`)

	// 3. Fetch all distinct (app_id, app_pubkey, day) tuples from impressions.
	// We query per-day because the current version may have changed between days.
	rows, err := adb.QueryContext(ctx, `SELECT DISTINCT app_id, app_pubkey, day FROM impressions`)
	if err != nil {
		panic(err)
	}
	defer rows.Close()

	updated, skipped := 0, 0
	for rows.Next() {
		var appID, appPubkey, day string
		if err := rows.Scan(&appID, &appPubkey, &day); err != nil {
			panic(err)
		}

		// 4. Parse the day and compute the end-of-day unix timestamp.
		// We want the newest kind 3063 for this app that existed on that day,
		// i.e. created_at <= end of day.
		t, err := time.Parse("2006-01-02", day)
		if err != nil {
			panic(fmt.Errorf("failed to parse day %q: %w", day, err))
		}
		endOfDay := nostr.Timestamp(t.UTC().Add(24*time.Hour - time.Second).Unix())

		// 5. Query the relay for the most recent kind 3063 for this app up to end of day.
		// The relay returns events ordered newest first, so the first result is what we want.
		filter := nostr.Filter{
			Kinds:   []int{events.KindAsset},
			Authors: []string{appPubkey},
			Tags:    nostr.TagMap{"i": []string{appID}},
			Until:   &endOfDay,
			Limit:   1,
		}

		assets, err := rdb.Query(ctx, filter)
		if err != nil {
			panic(err)
		}
		if len(assets) == 0 {
			skipped++
			slog.Info("no asset found for impression", "app_id", appID, "app_pubkey", appPubkey, "day", day)
			continue
		}

		appVersion, ok := events.Find(assets[0].Tags, "version")
		if !ok {
			skipped++
			slog.Info("no version tag in asset", "app_id", appID, "app_pubkey", appPubkey, "day", day, "asset", assets[0].ID)
			continue
		}

		// 6. Backfill all rows for this (app_id, app_pubkey, day) tuple
		if _, err = adb.ExecContext(ctx,
			`UPDATE impressions SET app_version = ? WHERE app_id = ? AND app_pubkey = ? AND day = ?`,
			appVersion, appID, appPubkey, day,
		); err != nil {
			panic(err)
		}
		updated++
	}

	fmt.Printf("done: updated=%d skipped=%d\n", updated, skipped)
}
