package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	path := flag.String("db", "", "path to blossom.db (required)")
	flag.Parse()

	if *path == "" {
		flag.Usage()
		os.Exit(1)
	}

	db, err := sql.Open("sqlite3", *path)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`ALTER TABLE blobs ADD COLUMN auth_pubkey TEXT`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migration failed (already applied?): %v\n", err)
		os.Exit(1)
	}

	fmt.Println("done: auth_pubkey column added to blobs table")
}
