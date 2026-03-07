---
description: Non-negotiable invariants — event integrity, security, availability, ACL correctness
alwaysApply: true
---

# relay — Invariants

## Event Integrity

- Events MUST be NIP-01 signature-verified before storage. Invalid signatures are rejected.
- Only configured allowed kinds are accepted. Unknown kinds are rejected.
- Filter specificity scoring MUST reject overly broad queries (prevents relay scraping).

## Access Control

- Blocked pubkeys MUST never be able to publish events or upload blobs, regardless of auth.
- ACL changes from CSV hot-reload MUST take effect on the next request — no stale cache.
- AppOwnership MUST enforce one publisher per app ID (kind 32267 `d` tag). The indexer pubkey can take over any app; any developer can reclaim from the indexer; other transitions are rejected.

## Security

- NIP-42 auth tokens must be validated before any write operation.
- NIP-98 auth tokens must be validated before any Blossom upload.
- Rate limiting MUST apply before auth checks — unauthenticated requests are still rate-limited.
- No private keys or secrets are ever logged.

## Availability

- The server must not crash on malformed WebSocket frames or invalid JSON.
- Blossom deduplication check must not block uploads — if CDN check fails, proceed with upload.
- Analytics writes are non-blocking (batched queue). A full queue drops events, never blocks requests.
- ACL file parse errors must log a warning and retain the previous ACL state — never clear it.

## Storage

- SQLite writes must use WAL mode for concurrent read performance.
- relay.db, blossom.db, analytics.db, and indexing.db are separate files — never share a connection pool.
- indexing.db is shared with zindex (both read/write). WAL mode with 5s busy_timeout handles contention.
