# WORK-001 — Demand-Driven App Discovery & Updates

**Feature:** FEAT-001-demand-driven-indexing.md
**Status:** Not Started

## Tasks

- [ ] 1. Create `pkg/indexing/store/` — schema and SQLite store
  - Files: `pkg/indexing/store/store.go`, `pkg/indexing/store/schema.sql`
  - Separate `indexing.db` file with WAL mode and busy_timeout (5s)
  - Discovery queue table:
    ```sql
    CREATE TABLE IF NOT EXISTS discovery_queue (
        url             TEXT PRIMARY KEY,
        request_count   INTEGER NOT NULL DEFAULT 1,
        first_seen_at   INTEGER NOT NULL,
        last_seen_at    INTEGER NOT NULL,
        status          TEXT NOT NULL DEFAULT 'pending',  -- pending | processing | done | failed
        checked_at      INTEGER
    );
    ```
  - Index status table (demand-driven update tracking):
    ```sql
    CREATE TABLE IF NOT EXISTS index_status (
        app_id              TEXT PRIMARY KEY,
        last_checked_at     INTEGER,
        last_requested_at   INTEGER NOT NULL,
        request_count       INTEGER NOT NULL DEFAULT 0,
        window_start        INTEGER NOT NULL
    );
    ```
  - Store methods: `UpsertDiscovery(url)`, `UpsertReleaseRequest(appID)`, `IsStale(appID, minTTL, maxTTL) bool`
  - Dynamic TTL logic in `IsStale`: `ttl = clamp(maxTTL / request_count, minTTL, maxTTL)` where minTTL=1h, maxTTL=7d
  - Notes: Both relay and zindex read/write this DB. WAL + 5s busy_timeout handles contention.

- [ ] 2. Create `pkg/indexing/` — engine with non-blocking queue writes
  - Files: `pkg/indexing/engine.go`, `pkg/indexing/config.go`
  - Follow analytics engine pattern: channels + select/default, background goroutine batches writes
  - `RecordDiscoveryMiss(url string)` — validates `https://github.com/` prefix, sends to channel
  - `RecordReleaseRequest(appID string)` — sends to channel; engine calls `IsStale` internally before writing
  - Config: queue channel size, min TTL (1h), max TTL (7d), max discovery queue size

- [ ] 3. Add repository URL search to the query builder
  - Files: `pkg/relay/store/store.go`
  - Detect `https://github.com/` prefix in the NIP-50 search term
  - When detected, query the `tags` table for exact `repository` tag match (no FTS)
  - Try both the URL as-is and with `.git` stripped (two bind params with OR)
  - If no GitHub URL prefix detected, fall through to existing FTS path unchanged
  - Notes: `repository` tag is already indexed in the tags table via `app_tags_ai` trigger

- [ ] 4. Wire indexing engine into server startup
  - Files: `cmd/main.go`, `pkg/config/config.go`
  - Create `indexing.db` at `{dataDir}/indexing.db`
  - Create indexing engine, pass to `relay.Setup`
  - Shut down engine on server close

- [ ] 5. Hook discovery miss recording into Query
  - Files: `pkg/relay/relay.go`
  - In `Query()`, after results returned: if filter is NIP-50 search on kind 32267 with `https://github.com/` URL and zero results → call `indexingEngine.RecordDiscoveryMiss(url)`
  - Non-blocking — must not affect query latency or return value

- [ ] 6. Hook staleness tracking into Query
  - Files: `pkg/relay/relay.go`
  - In `Query()`, after results returned: if filter includes kind 30063 or 3063 and results exist, extract app identifier from `i` tag filter or from returned events → call `indexingEngine.RecordReleaseRequest(appID)`
  - Non-blocking

- [ ] 7. Update `relay.Setup` signature
  - Files: `pkg/relay/relay.go`
  - Add optional `*indexing.Engine` parameter to `Setup` and the `Query` closure
  - nil = no indexing (backward compat for tests)

- [ ] 8. Replace `AppAlreadyExists` with `AppOwnership`
  - Files: `pkg/relay/relay.go`, `pkg/config/config.go`
  - Remove the current `AppAlreadyExists` function
  - Add `IndexerPubkey` to relay config (loaded from env var or hardcoded constant)
  - New `AppOwnership(store, indexerPubkey)` reject function for kind 32267:
    1. Extract `d` tag from event
    2. Query event store: does another pubkey have a kind 32267 with this `d` tag?
    3. If no existing event → accept (new app)
    4. If existing event from same pubkey → accept (NIP-33 replace)
    5. If new event is from indexer pubkey and existing is from a non-indexer → **delete** existing kind 32267 from event store → accept (indexer takeover)
    6. If existing event is from indexer pubkey and new event is from a non-indexer → **delete** indexer's kind 32267 from event store → accept (developer reclaim)
    7. Otherwise → reject (`ErrAppAlreadyExists`)
  - The delete operation removes the old kind 32267 event (and its tags) from the SQLite store. This is a relay-operator decision, not a Nostr protocol deletion.
  - Notes: The relay does NOT enforce the 14-day grace period. It trusts that zindex only attempts a takeover after the grace period. The relay's job is purely "who is allowed to hold this d-tag."

- [ ] 9. Tests
  - Files: `pkg/relay/store/store_test.go`, `pkg/indexing/store/store_test.go`, `pkg/indexing/engine_test.go`, `pkg/relay/relay_test.go`
  - Repository URL search: exact match on `repository` tag, `.git` normalization, no match returns empty, non-GitHub URL uses FTS
  - Discovery queue: upsert increments count, dedup by URL, invalid URL rejected
  - Staleness: TTL = clamp(7d / count, 1h, 7d); 1 request → 7d TTL; 168 requests → 1h TTL; 1000 requests → still 1h
  - Engine: non-blocking writes, channel full drops gracefully, nil engine is safe no-op
  - AppOwnership: new app accepted; same-pubkey update accepted; indexer takeover deletes old + accepts; developer reclaim deletes indexer's + accepts; non-indexer-to-non-indexer rejected

- [x] 10. Rename `server` → `relay` ✓ DONE
  - Renamed directory from `server/` to `relay/`
  - Updated `go.mod`: `module github.com/zapstore/relay`
  - Replaced all internal imports across 17 Go files (~45 occurrences)
  - Updated `.env.example` and `README.md` references
  - Removed rename notes from FEAT-001 and WORK-001
  - No external consumers found (zindex, zsp, webapp do not import the module)
  - GitHub repo rename (remote) still pending — do when ready to push

- [ ] 11. Update project documentation
  - Files: `README.md`, `AGENTS.md`
  - Update README.md: document `indexing.db` in data directory structure, add `IndexerPubkey` to config, document AppOwnership behavior, update repository URL search capability
  - Update AGENTS.md: if any key commands changed

- [ ] 12. Self-review against INVARIANTS.md
  - Queue writes non-blocking (availability invariant)
  - `indexing.db` separate from `relay.db` (storage invariant)
  - No external calls in request handlers (availability invariant)
  - AppOwnership deletes are relay-operator-level store management, not protocol-level deletions

## Test Coverage

| Scenario | Expected | Status |
|----------|----------|--------|
| Search `https://github.com/user/repo` matches repository tag | App returned | [ ] |
| Search `https://github.com/user/repo.git` matches tag without `.git` | App returned | [ ] |
| Search URL with no match queues discovery entry | Entry in discovery_queue | [ ] |
| Repeated miss for same URL bumps request_count | Count incremented, single row | [ ] |
| Non-GitHub-URL search uses FTS as before | FTS results returned | [ ] |
| Release request, app checked 30 min ago (below 1h floor) | Not queued | [ ] |
| Release request, app checked 2h ago, request_count=1 (TTL=7d) | Not queued | [ ] |
| Release request, app checked 2h ago, request_count=168 (TTL=1h) | Queued | [ ] |
| Channel full on queue write | Write dropped, query unaffected | [ ] |
| nil indexing engine | No panic, query works normally | [ ] |
| AppOwnership: new app (no existing 32267) | Accepted | [ ] |
| AppOwnership: same pubkey update | Accepted (NIP-33 replace) | [ ] |
| AppOwnership: indexer takes over from developer | Old 32267 deleted, new accepted | [ ] |
| AppOwnership: developer reclaims from indexer | Indexer's 32267 deleted, new accepted | [ ] |
| AppOwnership: developer A vs developer B | Rejected | [ ] |

## Decisions

### 2026-03-06 — Remove cadence system, pure demand-driven TTL

**Context:** zindex previously used a cadence ladder (1h/6h/12h/1d/3d/7d) per app. This is replaced entirely by demand-driven TTL.
**Decision:** TTL = clamp(7d / request_count_in_window, 1h, 7d). No cadence field needed.
**Rationale:** Demand signal is a better proxy for "how fresh does this need to be" than any static schedule. Popular apps stay fresh automatically; abandoned apps are checked at most weekly.

### 2026-03-06 — AppOwnership replaces AppAlreadyExists

**Context:** The indexer needs to take over stale self-published apps to ensure users always get updates. The current `AppAlreadyExists` check blocks this by rejecting any kind 32267 if another pubkey already has one with the same d-tag.
**Decision:** Replace with `AppOwnership` that allows role-based transitions: indexer can take over any app, developers can always reclaim from the indexer, other transitions are rejected. The relay does not enforce grace periods — zindex handles that.
**Rationale:** One publisher per app ID is the right constraint for now (avoids multi-publisher resolution in clients). The indexer is a trusted operator — the relay can trust that zindex only takes over after the grace period. Developer reclaim is instant because developer publishing is always preferred.

### 2026-03-06 — Rename server to relay

**Context:** The `server` name is generic. The project is specifically a Nostr relay (with Blossom CDN). The name should reflect this.
**Decision:** Rename directory, GitHub repo, and Go module from `server` to `relay`.
**Rationale:** Clarity. No `package server` exists in the codebase; all packages use their leaf directory names. The rename is mechanical.

## Spec Issues

_None_

## Progress Notes

_Not started_
