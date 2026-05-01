package main

import (
	"database/sql"
	"flag"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	analyticsDB := flag.String("analytics-db", "", "path to analytics sqlite db")
	flag.Parse()

	if *analyticsDB == "" {
		fmt.Println("analytics-db is required")
		return
	}

	db, err := sql.Open("sqlite3", *analyticsDB)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	// Copy all rows from downloads into app_downloads, mapping columns explicitly.
	// The old downloads table has columns added via ALTER TABLE in a different order,
	// and is missing 'type' from its PRIMARY KEY. We remap everything cleanly here.
	_, err = db.Exec(`
		INSERT OR IGNORE INTO app_downloads (hash, app_id, app_version, app_pubkey, day, source, type, country_code, count)
		SELECT hash, app_id, app_version, app_pubkey, day, source, type, country_code, count
		FROM downloads
	`)
	if err != nil {
		panic(fmt.Errorf("failed to copy downloads: %w", err))
	}

	var src, dst int
	db.QueryRow(`SELECT COUNT(*) FROM downloads`).Scan(&src)
	db.QueryRow(`SELECT COUNT(*) FROM app_downloads`).Scan(&dst)
	fmt.Printf("done: downloads=%d app_downloads=%d\n", src, dst)
}
