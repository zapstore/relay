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

	// 2. Add the new column if not already there (idempotent)
	adb.Exec(`ALTER TABLE app_impressions ADD COLUMN app_version TEXT NOT NULL DEFAULT ''`)

	// 3. Recreate the impressions table with app_version in the PRIMARY KEY if needed.
	// SQLite does not support ALTER TABLE to change primary keys.
	var pk string
	if err := adb.QueryRow(`SELECT pk FROM pragma_table_info('app_impressions') WHERE name = 'app_version'`).Scan(&pk); err != nil {
		panic(fmt.Errorf("check impressions primary key: %w", err))
	}
	if pk == "0" {
		_, err := adb.Exec(`
			CREATE TABLE app_impressions_new (
				app_id		 TEXT NOT NULL,
				app_pubkey	 TEXT NOT NULL,
				app_version	 TEXT NOT NULL DEFAULT '',
				day			 DATE NOT NULL,
				source		 TEXT NOT NULL,
				type		 TEXT NOT NULL,
				country_code TEXT,
				count		 INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (app_id, app_pubkey, app_version, day, source, type, country_code)
			);
			INSERT INTO app_impressions_new SELECT app_id, app_pubkey, '', day, source, type, country_code, count FROM app_impressions;
			DROP TABLE app_impressions;
			ALTER TABLE app_impressions_new RENAME TO app_impressions;
		`)
		if err != nil {
			panic(fmt.Errorf("recreate impressions with new primary key: %w", err))
		}
		fmt.Println("recreated impressions table with app_version in primary key")
	}

	// 3. Fetch all distinct (app_id, app_pubkey, day) tuples from impressions.
	// We query per-day because the current version may have changed between days.
	rows, err := adb.QueryContext(ctx, `SELECT DISTINCT app_id, app_pubkey, day FROM app_impressions`)
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
		day = normalizeDay(day)

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
			`UPDATE app_impressions SET app_version = ? WHERE app_id = ? AND app_pubkey = ? AND day = ?`,
			appVersion, appID, appPubkey, day,
		); err != nil {
			panic(err)
		}
		updated++
	}

	fmt.Printf("done: updated=%d skipped=%d\n", updated, skipped)
}

// normalizeDay truncates the day string to 10 characters if it exceeds that length.
// This is done because Sqlite returns the day as a string with the time included,
// e.g. "2023-01-01 12:00:00".
func normalizeDay(day string) string {
	if len(day) > 10 {
		return day[:10]
	}
	return day
}
