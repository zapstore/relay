package policy

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/zapstore/relay/pkg/events"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE events (
		id TEXT PRIMARY KEY,
		pubkey TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		kind INTEGER NOT NULL,
		tags TEXT NOT NULL,
		content TEXT NOT NULL,
		sig TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	store, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestAffectedForgeBan(t *testing.T) {
	pub := "aa"
	other := "bb"
	rows := []Listing{{
		ID: "app1", Pubkey: pub, Kind: events.KindApp,
		Tags: nostr.Tags{{"d", "com.example.spy"}, {"repository", "https://github.com/bad/spy"}},
	}, {
		ID: "rel1", Pubkey: pub, Kind: events.KindRelease,
		Tags: nostr.Tags{{"i", "com.example.spy"}},
	}, {
		ID: "apk1", Pubkey: pub, Kind: events.KindAsset,
		Tags: nostr.Tags{{"i", "com.example.spy"}, {"x", "abc"}, {"apk_certificate_hash", "cc"}},
	}, {
		ID: "app2", Pubkey: other, Kind: events.KindApp,
		Tags: nostr.Tags{{"d", "com.example.ok"}, {"repository", "https://github.com/good/ok"}},
	}}
	ids, hashes, apps := Affected(Publisher{Kind: KindForge, ID: "github.com/bad"}, rows)
	if len(ids) != 3 || len(hashes) != 1 || hashes[0] != "abc" || len(apps) != 1 || apps[0] != "com.example.spy" {
		t.Fatalf("ids %v hashes %v apps %v", ids, hashes, apps)
	}
}

func TestAffectedReleaseUsesAppID(t *testing.T) {
	pub := strings.Repeat("aa", 32)
	rows := []Listing{{
		ID: "app1", Pubkey: pub, Kind: events.KindApp,
		Tags: nostr.Tags{{"d", "com.example.spy"}},
	}, {
		ID: "rel1", Pubkey: pub, Kind: events.KindRelease,
		Tags: nostr.Tags{{"d", "com.example.spy@1.0.0"}, {"i", "com.example.spy"}},
	}, {
		ID: "apk1", Pubkey: pub, Kind: events.KindAsset,
		Tags: nostr.Tags{{"i", "com.example.spy"}, {"x", "abc"}},
	}}
	_, _, apps := Affected(Publisher{Kind: KindNostr, ID: pub}, rows)
	if len(apps) != 1 || apps[0] != "com.example.spy" {
		t.Fatalf("apps %v", apps)
	}
}

func TestEventVerdictBannedHashRejectsWithoutBanning(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	hash := strings.Repeat("ab", 32)
	signer := strings.Repeat("22", 32)
	cert := strings.Repeat("33", 32)
	if err := store.RecordBan(ctx, Publisher{Kind: KindNostr, ID: strings.Repeat("11", 32)}, "bad file", []string{hash}, nil); err != nil {
		t.Fatal(err)
	}
	verdict, err := store.EventVerdict(ctx, &nostr.Event{
		Kind:   events.KindAsset,
		PubKey: signer,
		Tags:   nostr.Tags{{"x", strings.ToUpper(hash)}, {"apk_certificate_hash", cert}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Reject || verdict.Reason != "bad file" {
		t.Fatalf("verdict %+v", verdict)
	}
	banned, err := store.Blacklisted(ctx, Publisher{Kind: KindNostr, ID: signer})
	if err != nil {
		t.Fatal(err)
	}
	if banned {
		t.Fatal("signer was blacklisted")
	}
}

func TestEventVerdictRejectsBlacklistedCert(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	cert := strings.Repeat("44", 32)
	if err := store.RecordBan(ctx, Publisher{Kind: KindCert, ID: cert}, "stolen cert", nil, nil); err != nil {
		t.Fatal(err)
	}
	verdict, err := store.EventVerdict(ctx, &nostr.Event{
		Kind:   events.KindAsset,
		PubKey: strings.Repeat("55", 32),
		Tags:   nostr.Tags{{"apk_certificate_hash", cert}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Reject || verdict.Reason != "" {
		t.Fatalf("verdict %+v", verdict)
	}
}

func TestPublishersAndHashes(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	pub, err := Normalize(KindForge, "github.com/bad")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBan(ctx, pub, "malware", []string{"abc"}, []string{"com.example.spy"}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Publishers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "github.com/bad" || rows[0].Reason != "malware" {
		t.Fatalf("publishers = %+v", rows)
	}
	hashes, err := store.Hashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 1 || hashes[0].Hash != "abc" {
		t.Fatalf("hashes = %+v", hashes)
	}
	if err := store.RemovePublisher(ctx, "forge", "github.com/bad"); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Publishers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("publishers after remove = %+v", rows)
	}
	hashes, err = store.Hashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 1 {
		t.Fatalf("hash should remain, got %+v", hashes)
	}
	if err := store.RemoveHash(ctx, "ABC"); err != nil {
		t.Fatal(err)
	}
	hashes, err = store.Hashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 0 {
		t.Fatalf("hashes after remove = %+v", hashes)
	}
}

func TestAllowNostrKey(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	hex := strings.Repeat("ab", 32)
	npub, err := nip19.EncodePublicKey(hex)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Allow(ctx, npub); err != nil {
		t.Fatal(err)
	}
	if err := store.Allow(ctx, strings.ToUpper(hex)); err != nil {
		t.Fatal(err)
	}
	ok, err := store.Allowed(ctx, hex)
	if err != nil || !ok {
		t.Fatalf("allowed %v %v", ok, err)
	}
	keys, err := store.AllowedKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].ID != hex {
		t.Fatalf("keys %+v %v", keys, err)
	}
	if err := store.Allow(ctx, "github.com/someone"); err == nil {
		t.Fatal("forge key accepted")
	}
	if err := store.Disallow(ctx, npub); err != nil {
		t.Fatal(err)
	}
	ok, err = store.Allowed(ctx, hex)
	if err != nil || ok {
		t.Fatalf("after disallow %v %v", ok, err)
	}
}
