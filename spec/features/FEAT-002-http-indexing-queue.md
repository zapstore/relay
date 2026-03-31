# FEAT-002 — HTTP Indexing Queue

## Goal

Let authenticated users suggest a GitHub repository URL for indexing via an HTTP endpoint. The URL is queued in `indexing.db` exactly like a NIP-50 search miss — zindex picks it up asynchronously. This gives clients a way to trigger discovery without a WebSocket connection.

## Non-Goals

- Synchronous zsp execution — the endpoint queues and returns immediately
- Status polling or webhooks for indexing progress
- Accepting non-repository URLs (only valid `/:user/:repo` paths)
- New database tables — reuses the existing `discovery_queue`

## User-Visible Behavior

- Client sends `POST /queue` with a GitHub URL and a NIP-98 auth header
- The relay validates the auth token, checks ACL (blocked/allowed/vertex), validates the URL shape, and queues it
- Returns `202 Accepted` — the URL is now pending in `discovery_queue`
- zindex picks it up on its next cycle, same as a search miss

## Wire Format

### Request

```
POST /queue
Authorization: Nostr <base64-encoded-kind-27235-event>
Content-Type: application/json

{"url": "https://github.com/user/repo"}
```

The NIP-98 auth event must have:
- `kind`: 27235
- `created_at`: within 60 seconds of now
- `u` tag: matching the request URL
- `method` tag: `POST`
- Valid signature

### Responses

| Status | Meaning |
|--------|---------|
| 202 | Queued (or bumped `request_count` if already known) |
| 400 | Missing/invalid URL or not a valid repository URL |
| 401 | Missing or invalid NIP-98 auth |
| 403 | Pubkey blocked by ACL |
| 429 | Rate limited |

## Request Flow

```
POST /queue → rate limiter → NIP-98 auth → ACL AllowPubkey → repourl.Parse → engine.RecordDiscoveryMiss → 202
```

## Hosting

The endpoint is mounted on the Blossom server's port via a shared `http.ServeMux` in `cmd/main.go`. No new port or config vars.

## ACL

Uses `acl.AllowPubkey(ctx, pubkey)` — the same function Blossom uploads use. This runs the full pipeline: blocked → allowed → unknown policy (allow all / block all / Vertex DVM).

## NIP-98 Auth

A shared `pkg/auth/` package (or inline helper) that:
1. Reads `Authorization: Nostr <base64>` header
2. Base64-decodes to a JSON nostr event
3. Verifies event signature and kind == 27235
4. Checks `u` tag matches request URL, `method` tag matches HTTP method
5. Rejects if `created_at` is more than 60 seconds old
6. Returns the authenticated pubkey

This can later be reused if other HTTP endpoints need NIP-98.

## Edge Cases

- **URL already queued as `pending` or `processing`:** `UpsertDiscovery` bumps `request_count` and `last_seen_at`. No duplicate work.
- **URL previously `done`:** `UpsertDiscovery` resets status to `pending` so zindex re-checks it.
- **Malformed URL:** `repourl.Parse` returns false → 400.
- **Replay attack:** The 60-second `created_at` window limits replay. The endpoint is idempotent so replays within the window are harmless.
- **Indexing engine nil:** If indexing.db failed to open at startup, the endpoint returns 503.

## Acceptance Criteria

- [ ] `POST /queue` with valid NIP-98 and GitHub URL returns 202
- [ ] Invalid/missing NIP-98 returns 401
- [ ] Blocked pubkey returns 403
- [ ] Unknown pubkey follows configured policy (allow/block/vertex)
- [ ] Invalid URL returns 400
- [ ] URL is written to `discovery_queue` via `RecordDiscoveryMiss`
- [ ] Rate limiting applies before auth
- [ ] No new database tables, ports, or env vars
- [ ] Endpoint is mounted on the Blossom port
