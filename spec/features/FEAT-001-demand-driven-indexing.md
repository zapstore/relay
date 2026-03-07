# FEAT-001 — Demand-Driven App Discovery & Updates

## Goal

Let user queries drive app discovery and update checks. When a user searches for a GitHub repository URL and nothing is found, queue it for background indexing. When a user requests releases for a known app, track staleness and queue update checks based on demand. Support indexer takeover of stale self-published apps via an AppOwnership mechanism that enforces one publisher per app ID.

## Non-Goals

- Multi-platform support (GitLab, Codeberg) — GitHub only until proven
- Searching GitHub's Search API — discovery is by exact repo URL only
- Placeholder or partial events — every event in the relay is fully validated
- Hot-path external calls — all indexing happens asynchronously via zindex
- Changes to Nostr protocol or NIP-82 event structure
- Multiple publishers per app ID on the relay — one publisher at a time

## User-Visible Behavior

### Discovery

- User searches for `https://github.com/user/repo` (with or without `.git`) via NIP-50 on kind 32267
- If a matching app exists (exact match on `repository` tag), it is returned immediately
- If no match, the URL is silently queued for background indexing in `indexing.db`
- zindex picks up the queue entry, runs zsp, publishes full NIP-82 events if valid
- The next search for the same URL returns the indexed app

### Updates

- User requests kind 30063 or 3063 for a known app
- The relay serves existing data from the DB immediately
- In the background, the relay checks when this app was last verified; if stale, it queues an update
- Staleness TTL is dynamic: `clamp(7d / request_count, 1h, 7d)` — more requests = shorter TTL, bounded between 1 hour and 7 days
- zindex picks up stale entries, runs zsp, publishes fresh events if new versions exist

### AppOwnership (replaces AppAlreadyExists)

The relay enforces one publisher per app ID (kind 32267 `d` tag). The current `AppAlreadyExists` reject check is replaced by `AppOwnership` with role-aware transitions:

| New publisher | Existing publisher | Result |
|---|---|---|
| same pubkey | same pubkey | accept (NIP-33 replace) |
| indexer | non-indexer | delete old kind 32267, accept (takeover) |
| non-indexer | indexer | delete old kind 32267, accept (developer reclaim) |
| anyone else | anyone else | reject |

The indexer pubkey is a known constant. The relay trusts that zindex has enforced the 14-day grace period before attempting a takeover — no staleness check in the relay itself.

A developer can always reclaim their app from the indexer instantly. Their next `zsp publish` replaces the indexer's kind 32267 and restores developer ownership.

## Edge Cases

- **GitHub API down:** Queue entries stay pending; retried on next zindex cycle. Relay serves existing data.
- **Repository doesn't exist:** zindex marks entry as `failed` with `checked_at`. Not retried until negative-result TTL expires.
- **Repository exists but no valid APK:** Same — marked as failed.
- **Malformed URL:** Relay validates URL format before queueing. Invalid URLs are silently ignored.
- **Queue overflow:** Discovery queue has a max size. Least-requested entries are evicted.
- **zindex down:** Queues grow but relay serves existing data. No degradation.
- **Concurrent requests for same stale app:** All bump request counter; only one update entry exists (deduplicated by app identifier).
- **zsp fails mid-processing:** Entry stays in queue for retry. No partial events published.
- **Repository tag format variance:** Existing 32267 events may store `https://github.com/user/repo`, `https://github.com/user/repo.git`, etc. Search normalizes both the query and stored values for comparison.
- **Indexer takeover while developer publishes simultaneously:** The developer's event wins — `AppOwnership` always lets a developer reclaim from the indexer. No race condition; the last writer wins at the NIP-33 level.
- **Developer reclaims then goes stale again:** 14-day grace period restarts from when zindex next detects an unpublished upstream version.

## Acceptance Criteria

- [ ] NIP-50 search for kind 32267 with a `https://github.com/` URL does exact match on `repository` tag
- [ ] URL searches that return zero results are queued in `indexing.db` discovery table
- [ ] Requests for kind 30063/3063 record app identifier + request count in `indexing.db` staleness table
- [ ] Staleness TTL adjusts dynamically based on request frequency (floor: 1 hour)
- [ ] Stale apps are queued for update in `indexing.db`
- [ ] Queue entries are deduplicated by identifier with request count tracking
- [ ] Failed entries have a negative-result TTL before retry
- [ ] Queue writes are non-blocking (channel + select/default)
- [ ] `indexing.db` is a separate SQLite file with WAL mode and 5s busy_timeout
- [ ] Existing relay query performance is unaffected
- [ ] `AppAlreadyExists` is replaced by `AppOwnership` with the role-aware transition table above
- [ ] Indexer pubkey can take over any app ID (kind 32267) — old event is deleted from store
- [ ] Any non-indexer pubkey can reclaim an app ID from the indexer — indexer's event is deleted
- [ ] Non-indexer-to-non-indexer transitions are still rejected

## Notes

- `indexing.db` is shared between relay (read/write) and zindex (read/write). WAL mode with 5s busy_timeout handles concurrency.
- The analytics engine's batched channel pattern (`pkg/analytics/engine.go`) is the model for non-blocking queue writes.
- Kind 3063 (Asset) events are regular (non-replaceable) and accumulate; a cleanup strategy for old versions is needed eventually.
- The `AppOwnership` check replaces `AppAlreadyExists` in the reject pipeline. It needs access to the event store to delete the old kind 32267 when a transition occurs. The indexer pubkey constant is defined in config.
- The Go module has been renamed from `github.com/zapstore/server` to `github.com/zapstore/relay`. The GitHub repo is renamed accordingly (GitHub auto-redirects the old URL).
