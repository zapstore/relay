package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
	"github.com/zapstore/relay/pkg/rate"
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

func TestSuggestSkipsBlacklistedForge(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	pub, err := Normalize(KindForge, "github.com/bad")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBan(ctx, pub, "malware", nil, nil); err != nil {
		t.Fatal(err)
	}
	id, stored, err := store.PutSuggestion(ctx, "https://github.com/bad/app", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if stored {
		t.Fatal("blacklisted forge was queued")
	}
	if id == "" {
		t.Fatal("missing id")
	}
	rows, err := store.Suggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("queue = %+v", rows)
	}
	again, _, err := store.PutSuggestion(ctx, "https://github.com/bad/app", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("id %s then %s", id, again)
	}
}

func TestSuggestAlwaysCreated(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	pub, err := Normalize(KindForge, "github.com/bad")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBan(ctx, pub, "malware", nil, nil); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/suggest", strings.NewReader(`{"repository":"https://github.com/bad/app"}`))
	rec := httptest.NewRecorder()
	Suggest(store, rate.NewLimiter(rate.NewConfig()))(rec, req)
	if rec.Code != http.StatusCreated || rec.Body.Len() != 0 {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
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

func TestSocketListAndRemove(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "relay.sock")
	errc := make(chan error, 1)
	go func() { errc <- Listen(ctx, path, store, nil) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	id, stored, err := store.PutSuggestion(context.Background(), "https://github.com/good/app", "", "")
	if err != nil || !stored {
		t.Fatal(err, stored)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte(`{"op":"remove","id":"` + id + "\"}\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) == "" || buf[0] != '{' {
		t.Fatalf("response %q", buf[:n])
	}
	conn.Close()
	rows, err := store.Suggestions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("queue = %+v", rows)
	}
	cancel()
	<-errc
}

func TestSocketCommands(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	path := filepath.Join(t.TempDir(), "relay.sock")
	var gotKind, gotID, gotReason string
	errc := make(chan error, 1)
	go func() {
		errc <- Listen(ctx, path, store, func(_ context.Context, pub Publisher, reason string) error {
			gotKind, gotID, gotReason = pub.Kind, pub.ID, reason
			return nil
		})
	}()
	waitSocket(t, path)

	key := strings.Repeat("ab", 32)
	if resp := socketCall(t, path, `{"op":"suggestions"}`); !resp.OK || len(resp.Suggestions) != 0 {
		t.Fatalf("suggestions %+v", resp)
	}
	id, stored, err := store.PutSuggestion(t.Context(), "https://github.com/good/app", "", "")
	if err != nil || !stored {
		t.Fatal(err, stored)
	}
	resp := socketCall(t, path, `{"op":"suggestions"}`)
	if !resp.OK || len(resp.Suggestions) != 1 || resp.Suggestions[0].ID != id {
		t.Fatalf("suggestions %+v", resp)
	}
	if resp := socketCall(t, path, `{"op":"remove","id":"`+id+`"}`); !resp.OK {
		t.Fatalf("remove %+v", resp)
	}
	rows, err := store.Suggestions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("queue = %+v", rows)
	}

	allow := `{"op":"allow","kind":"nostr","id":"` + key + `","reason":"paid"}`
	if resp := socketCall(t, path, allow); !resp.OK {
		t.Fatalf("allow %+v", resp)
	}
	resp = socketCall(t, path, `{"op":"allowed"}`)
	if !resp.OK || len(resp.Allowed) != 1 || resp.Allowed[0].ID != key || resp.Allowed[0].Kind != KindNostr {
		t.Fatalf("allowed %+v", resp)
	}
	if resp := socketCall(t, path, `{"op":"disallow","kind":"nostr","id":"`+key+`"}`); !resp.OK {
		t.Fatalf("disallow %+v", resp)
	}
	if resp := socketCall(t, path, `{"op":"allowed"}`); !resp.OK || len(resp.Allowed) != 0 {
		t.Fatalf("allowed after disallow %+v", resp)
	}

	ban := `{"op":"ban","kind":"nostr","id":"` + key + `","reason":"spam"}`
	if resp := socketCall(t, path, ban); !resp.OK {
		t.Fatalf("ban %+v", resp)
	}
	if gotKind != KindNostr || gotID != key || gotReason != "spam" {
		t.Fatalf("ban recorded %s %s %s", gotKind, gotID, gotReason)
	}
	if resp := socketCall(t, path, `{"op":"nope"}`); resp.OK || resp.Error == "" {
		t.Fatalf("unknown %+v", resp)
	}

	cancel()
	<-errc
}

func waitSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
}

type socketResponse struct {
	OK          bool         `json:"ok"`
	Error       string       `json:"error,omitempty"`
	Suggestions []Suggestion `json:"suggestions"`
	Allowed     []AllowedRow `json:"allowed"`
}

func socketCall(t *testing.T, path, line string) socketResponse {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
	var resp socketResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSuggestErrors(t *testing.T) {
	store := testStore(t)
	lim := rate.NewLimiter(rate.NewConfig())
	h := Suggest(store, lim)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/suggest", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Body.Len() != 0 {
		t.Fatalf("method %d body %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/suggest", strings.NewReader(`{`)))
	if rec.Code != http.StatusCreated || rec.Body.Len() != 0 {
		t.Fatalf("json %d body %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/suggest", strings.NewReader(`{"repository":"not a url"}`)))
	if rec.Code != http.StatusCreated || rec.Body.Len() != 0 {
		t.Fatalf("url %d body %q", rec.Code, rec.Body.String())
	}
	rows, err := store.Suggestions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("discarded url was queued: %+v", rows)
	}

	for lim.Allow("192.0.2.1", 1) {
	}
	req := httptest.NewRequest(http.MethodPost, "/suggest", strings.NewReader(`{"repository":"https://github.com/good/app"}`))
	req.RemoteAddr = "192.0.2.1:1234"
	rec = httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Body.Len() != 0 {
		t.Fatalf("rate %d body %q", rec.Code, rec.Body.String())
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

func TestSuggestNormalizesScheme(t *testing.T) {
	store := testStore(t)
	bare, stored, err := store.PutSuggestion(context.Background(), "github.com/good/app", "", "")
	if err != nil || !stored {
		t.Fatal(err, stored)
	}
	full, stored, err := store.PutSuggestion(context.Background(), "https://github.com/good/app", "", "")
	if err != nil || !stored {
		t.Fatal(err, stored)
	}
	if bare != full {
		t.Fatalf("id %s then %s", bare, full)
	}
	rows, err := store.Suggestions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Repository != "https://github.com/good/app" {
		t.Fatalf("rows %+v", rows)
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
