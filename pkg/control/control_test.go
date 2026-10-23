package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/zapstore/relay/pkg/catalog"
	"github.com/zapstore/relay/pkg/policy"
)

var relayKey = strings.Repeat("11", 32)

type harness struct {
	path    string
	db      *sql.DB
	store   *policy.Store
	data    string
	admin   string // secret key
	mu      sync.Mutex
	bans    []string
	sealed  chan SealRequest
	release chan struct{}
}

func (h *harness) banned() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bans...)
}

func newHarness(t *testing.T, readOnly bool) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE events (id TEXT PRIMARY KEY, pubkey TEXT, created_at INTEGER, kind INTEGER, tags TEXT, content TEXT, sig TEXT)`); err != nil {
		t.Fatal(err)
	}
	store, err := policy.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("", "ctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	h := &harness{
		path: filepath.Join(sockDir, "r.sock"), db: db, store: store, data: dir,
		admin: nostr.GeneratePrivateKey(), sealed: make(chan SealRequest, 4), release: make(chan struct{}),
	}
	adminPub, _ := nostr.GetPublicKey(h.admin)
	s := &Server{
		Store: store, Data: dir, Relay: relayKey, Admins: []string{adminPub}, ReadOnly: readOnly,
		Ban: func(_ context.Context, pub policy.Publisher, reason string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.bans = append(h.bans, pub.Kind+":"+pub.ID+":"+reason)
			return nil
		},
		Seal: func(ctx context.Context, req SealRequest) (string, error) {
			h.sealed <- req
			select {
			case <-h.release:
				return "sealed", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
		Enrich: func(context.Context, string) (string, error) { return "", nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Listen(ctx, h.path) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.Dial("unix", h.path)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return h
}

func (h *harness) call(t *testing.T, cmd any) response {
	t.Helper()
	conn, err := net.Dial("unix", h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	line, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	var resp response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func (h *harness) signed(t *testing.T, secret, op string, a args, at time.Time) map[string]any {
	t.Helper()
	ev := nostr.Event{Kind: AuthKind, CreatedAt: nostr.Timestamp(at.Unix()), Tags: commandTags(relayKey, op, a), Content: ""}
	if err := ev.Sign(secret); err != nil {
		t.Fatal(err)
	}
	cmd := map[string]any{"op": op, "event": ev}
	for k, v := range a {
		cmd[k] = v
	}
	return cmd
}

func asOtherUser(t *testing.T) {
	t.Helper()
	own := ownUID
	ownUID = func() int { return os.Getuid() + 1 }
	t.Cleanup(func() { ownUID = own })
}

func TestOperatorCommands(t *testing.T) {
	h := newHarness(t, false)
	key := strings.Repeat("ab", 32)

	if resp := h.call(t, map[string]string{"op": "ban", "kind": "nostr", "id": strings.ToUpper(key), "reason": "spam"}); !resp.OK {
		t.Fatalf("ban %+v", resp)
	}
	if got := h.banned(); len(got) != 1 || got[0] != "nostr:"+key+":spam" {
		t.Fatalf("bans %v", got)
	}
	if resp := h.call(t, map[string]string{"op": "nope"}); resp.OK || resp.Error == "" {
		t.Fatalf("unknown %+v", resp)
	}
	if resp := h.call(t, map[string]string{"op": "allow", "id": key, "extra": "x"}); resp.OK {
		t.Fatalf("unknown field accepted %+v", resp)
	}
	npub, _ := nip19.EncodePublicKey(key)
	if resp := h.call(t, map[string]string{"op": "allow", "kind": "nostr", "id": npub}); !resp.OK {
		t.Fatalf("allow %+v", resp)
	}
	listed := h.call(t, map[string]string{"op": "allowed"})
	if !listed.OK || listed.Allowed == nil || len(*listed.Allowed) != 1 || (*listed.Allowed)[0] != key {
		t.Fatalf("allowed %+v", listed)
	}
	if resp := h.call(t, map[string]string{"op": "allow", "kind": "forge", "id": "github.com/x"}); resp.OK {
		t.Fatalf("forge allow %+v", resp)
	}
	if resp := h.call(t, map[string]string{"op": "disallow", "id": key}); !resp.OK {
		t.Fatalf("disallow %+v", resp)
	}
}

func TestUnsignedWriteFromAnotherUserIsRefused(t *testing.T) {
	h := newHarness(t, false)
	asOtherUser(t)
	resp := h.call(t, map[string]string{"op": "allow", "id": strings.Repeat("ab", 32)})
	if resp.OK || !strings.Contains(resp.Error, "relay's own user") {
		t.Fatalf("allow %+v", resp)
	}
	if resp := h.call(t, map[string]string{"op": "allowed"}); !resp.OK {
		t.Fatalf("a read is open to the socket group %+v", resp)
	}
	if resp := h.call(t, map[string]string{"op": "jobs"}); !resp.OK || resp.Jobs == nil {
		t.Fatalf("jobs %+v", resp)
	}
}

func TestSignedCommands(t *testing.T) {
	h := newHarness(t, false)
	asOtherUser(t)
	key := strings.Repeat("cd", 32)
	now := time.Now()

	cmd := h.signed(t, h.admin, "allow", args{"id": key}, now)
	if resp := h.call(t, cmd); !resp.OK {
		t.Fatalf("signed allow %+v", resp)
	}
	if ok, _ := h.store.Allowed(context.Background(), key); !ok {
		t.Fatal("key not allowed")
	}

	stranger := nostr.GeneratePrivateKey()
	if resp := h.call(t, h.signed(t, stranger, "disallow", args{"id": key}, now)); resp.OK || !strings.Contains(resp.Error, "not a relay admin") {
		t.Fatalf("stranger %+v", resp)
	}
	if resp := h.call(t, h.signed(t, h.admin, "disallow", args{"id": key}, now.Add(-2*time.Minute))); resp.OK || !strings.Contains(resp.Error, "too old") {
		t.Fatalf("stale %+v", resp)
	}

	swapped := h.signed(t, h.admin, "disallow", args{"id": key}, now)
	swapped["op"] = "ban"
	if resp := h.call(t, swapped); resp.OK {
		t.Fatalf("op swapped %+v", resp)
	}
	other := h.signed(t, h.admin, "disallow", args{"id": key}, now)
	other["id"] = strings.Repeat("ef", 32)
	if resp := h.call(t, other); resp.OK {
		t.Fatalf("args swapped %+v", resp)
	}

	tampered := h.signed(t, h.admin, "disallow", args{"id": key}, now)
	ev := tampered["event"].(nostr.Event)
	ev.Tags = commandTags(relayKey, "disallow", args{"id": strings.Repeat("ef", 32)})
	tampered["event"] = ev
	delete(tampered, "id")
	if resp := h.call(t, tampered); resp.OK || !strings.Contains(resp.Error, "event id") {
		t.Fatalf("tampered %+v", resp)
	}

	ev = nostr.Event{Kind: AuthKind, CreatedAt: nostr.Now(), Tags: commandTags(strings.Repeat("22", 32), "disallow", args{"id": key})}
	_ = ev.Sign(h.admin)
	if resp := h.call(t, map[string]any{"event": ev}); resp.OK || !strings.Contains(resp.Error, "do not match") {
		t.Fatalf("other relay %+v", resp)
	}

	if ok, _ := h.store.Allowed(context.Background(), key); !ok {
		t.Fatal("a refused command changed the whitelist")
	}
}

func TestReadOnlyRefusesPolicyChanges(t *testing.T) {
	h := newHarness(t, true)
	key := strings.Repeat("ab", 32)
	if resp := h.call(t, map[string]string{"op": "ban", "kind": "nostr", "id": key, "reason": "spam"}); resp.OK || resp.Error != "relay is read-only" {
		t.Fatalf("ban %+v", resp)
	}
	if len(h.banned()) != 0 {
		t.Fatal("ban ran")
	}
	if resp := h.call(t, h.signed(t, h.admin, "allow", args{"id": key}, time.Now())); resp.OK || resp.Error != "relay is read-only" {
		t.Fatalf("signed allow %+v", resp)
	}
	if resp := h.call(t, map[string]string{"op": "allowed"}); !resp.OK {
		t.Fatalf("allowed %+v", resp)
	}
}

func TestSealJobAndCancel(t *testing.T) {
	h := newHarness(t, false)
	asOtherUser(t)
	resp := h.call(t, h.signed(t, h.admin, "seal", args{"filter": "org.signal", "note": "test"}, time.Now()))
	if !resp.OK || resp.Job == nil || !resp.Job.Running {
		t.Fatalf("seal %+v", resp)
	}
	if req := <-h.sealed; req.Filter != "org.signal" || req.HasFrom {
		t.Fatalf("seal request %+v", req)
	}
	jobs := h.call(t, map[string]string{"op": "jobs", "kind": "seal"})
	if !jobs.OK || len(*jobs.Jobs) != 1 || (*jobs.Jobs)[0].Note != "test" {
		t.Fatalf("jobs %+v", jobs)
	}
	busy := h.call(t, h.signed(t, h.admin, "seal", args{}, time.Now()))
	if busy.OK || busy.Error != errBusy.Error() {
		t.Fatalf("second seal %+v", busy)
	}
	id := resp.Job.ID
	if stop := h.call(t, h.signed(t, h.admin, "cancel", args{"job": jsonInt(id)}, time.Now())); !stop.OK {
		t.Fatalf("cancel %+v", stop)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		jobs = h.call(t, map[string]string{"op": "jobs"})
		if j := (*jobs.Jobs)[0]; !j.Running {
			if !j.Stopped || j.Error == "" {
				t.Fatalf("stopped job %+v", j)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSealFromNeedsAnOlderSnapshot(t *testing.T) {
	h := newHarness(t, false)
	resp := h.call(t, map[string]string{"op": "seal", "from": "0"})
	if resp.OK || !strings.Contains(resp.Error, "older than the current snapshot") {
		t.Fatalf("seal from %+v", resp)
	}
	if resp := h.call(t, map[string]string{"op": "seal", "from": "1", "filter": "x"}); resp.OK {
		t.Fatalf("filter with from %+v", resp)
	}
}

func TestDeleteBundle(t *testing.T) {
	h := newHarness(t, false)
	path := catalog.BundlePath(h.data, 3, 7)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if resp := h.call(t, map[string]string{"op": "delete-bundle", "from": "3", "to": "7"}); !resp.OK {
		t.Fatalf("delete %+v", resp)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("bundle still there: %v", err)
	}
	for _, bad := range []map[string]string{
		{"op": "delete-bundle", "from": "7", "to": "3"},
		{"op": "delete-bundle", "from": "../x", "to": "3"},
		{"op": "delete-bundle", "from": "03", "to": "7"},
	} {
		if resp := h.call(t, bad); resp.OK {
			t.Fatalf("%v accepted", bad)
		}
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
