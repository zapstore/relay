package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pippellia-btc/blossom"
)

var ctx = context.Background()

func TestSaveAndQuery(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	want := BlobMeta{
		Hash:      blossom.ComputeHash([]byte("test blob content")),
		Type:      "application/octet-stream",
		Size:      1024,
		CreatedAt: time.Now().UTC().Truncate(time.Second), // SQLite stores seconds only
	}

	// First save should insert
	inserted, err := store.Save(ctx, want)
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if !inserted {
		t.Error("expected inserted=true for new blob")
	}

	// Second save should not insert (already exists)
	inserted, err = store.Save(ctx, want)
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if inserted {
		t.Error("expected inserted=false for existing blob")
	}

	has, err := store.Has(ctx, want.Hash)
	if err != nil {
		t.Fatalf("Has failed: %v", err)
	}
	if !has {
		t.Fatalf("blob should exist, but doesn't")
	}

	got, err := store.Query(ctx, want.Hash)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected blobmeta %v, got %v", want, got)
	}
}
