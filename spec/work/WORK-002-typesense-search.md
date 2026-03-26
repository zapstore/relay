# WORK-002 — Typesense Hybrid Search (NIP-50)

**Feature:** NIP-50 semantic/hybrid search for kind 32267 app events
**Status:** In Progress

## Context

Current search: SQLite FTS5 with trigram tokenizer + BM25 ranking. Handles exact/partial
keyword matches but no typo tolerance and no semantic understanding.

Typesense replaces the FTS5 path with hybrid search (keyword BM25 + semantic vector via
`all-MiniLM-L12-v2`, rank-fused). FTS5 is retained as a fallback when Typesense is
unavailable or disabled.

Typesense is a single static binary — no containers, no external services beyond the
process itself.

## Tasks

- [x] 1. Write work packet
  - Files: `spec/work/WORK-002-typesense-search.md`

- [ ] 2. `pkg/search/config.go` — Config struct
  - `TYPESENSE_URL` (default `http://localhost:8108`)
  - `TYPESENSE_API_KEY` (required when enabled)
  - `TYPESENSE_ENABLED` (default false)
  - Files: `pkg/search/config.go`

- [ ] 3. `pkg/search/search.go` — Engine
  - `New(config, db *sql.DB) (*Engine, error)` — creates client, ensures collection exists,
    starts async worker goroutine, triggers backfill
  - `Index(event *nostr.Event)` — non-blocking channel send
  - `Delete(eventID string)` — non-blocking channel send
  - `Search(ctx, filter nostr.Filter) ([]string, error)` — returns event IDs ordered by
    relevance; translates filter.Authors/Tags/Since/Until into Typesense filter_by
  - `Backfill(ctx)` — paginated SELECT from `events WHERE kind=32267`, bulk upsert to
    Typesense in batches of 100; runs once at startup in a goroutine
  - `Close()` — drains worker channel, closes client
  - Files: `pkg/search/search.go`

- [ ] 4. `pkg/relay/store/store.go` — search routing in queryBuilder
  - Accept optional `*search.Engine` via `SetSearchEngine()`
  - When search term present AND engine non-nil: call `engine.Search()` → fetch events
    from SQLite by returned IDs (preserving relevance order)
  - Fall through to existing `appSearchQuery` (FTS5) only on Typesense error
  - Files: `pkg/relay/store/store.go`

- [ ] 5. `pkg/relay/relay.go` — wire engine into Save
  - `Save` receives `*search.Engine` (nil = disabled)
  - After successful db.Save/Replace for kind 32267: `engine.Index(event)` (non-blocking)
  - After successful db.Delete for kind 32267: `engine.Delete(id)` (non-blocking)
  - Files: `pkg/relay/relay.go`

- [ ] 6. `pkg/config/config.go` — add Search field
  - Files: `pkg/config/config.go`

- [ ] 7. `cmd/` — instantiate engine, pass to relay.Setup
  - Files: `cmd/server/main.go` (or wherever Setup is called)

- [ ] 8. `pkg/search/search_test.go` — tests
  - TestSearchDisabled: nil engine falls through to FTS5 (no Typesense call)
  - TestIndexDelete: channel sends are non-blocking even when full
  - TestBackfillBatching: verifies pagination logic with mock DB rows
  - TestSearchFilterTranslation: filter.Authors/Tags/Since/Until → Typesense filter_by string
  - Files: `pkg/search/search_test.go`

- [ ] 9. Self-review against INVARIANTS.md

## Backfill Design

```
startup
  └─ go engine.backfill(ctx)
       ├─ SELECT id,pubkey,created_at,kind,tags,content,sig
       │  FROM events WHERE kind=32267
       │  ORDER BY created_at
       │  LIMIT 100 OFFSET 0  (repeat until 0 rows)
       ├─ marshal each row → AppDocument{id, name, summary, content}
       ├─ client.Collection("apps").Documents().Import(batch, &ImportParams{Action:"upsert"})
       └─ log progress every 500 docs; log completion
```

Idempotent: upsert means re-running on a populated Typesense is safe.
Non-blocking: runs in a goroutine; relay serves requests immediately.
Backpressure: if Typesense is slow, backfill slows too — no relay impact.

## Collection Schema

```json
{
  "name": "apps",
  "fields": [
    {"name": "id",      "type": "string"},
    {"name": "name",    "type": "string"},
    {"name": "summary", "type": "string", "optional": true},
    {"name": "content", "type": "string", "optional": true},
    {"name": "embedding", "type": "float[]", "num_dim": 384,
     "embed": {
       "from": ["name", "summary", "content"],
       "model_config": {"model_name": "ts/all-MiniLM-L12-v2"}
     }}
  ],
  "token_separators": ["-", "_", "."]
}
```

`token_separators` handles reverse-domain app IDs like `com.example.app` and
platform strings like `android-arm64-v8a` in the name field.

## Search Query

Hybrid search with equal weight (alpha=0.5) between keyword and semantic:

```
q=<search term>
query_by=name,summary,content,embedding
vector_query=embedding:([], alpha: 0.5)
exclude_fields=embedding
per_page=<filter.Limit>
filter_by=<translated from filter.Authors/Tags/Since/Until>
```

## Filter Translation

| nostr.Filter field | Typesense filter_by |
|---|---|
| `filter.Authors` | `pubkey:[pk1,pk2]` |
| `filter.Since` | `created_at:>={unix}` |
| `filter.Until` | `created_at:<={unix}` |
| `filter.Tags["t"]` | `tags:[val1,val2]` |

Note: `filter.Tags["f"]` (platform) is not stored in Typesense — platform filtering
remains in SQLite after IDs are returned. This is intentional: platform is a structured
filter, not a search concern.

## Test Coverage

| Scenario | Expected | Status |
|---|---|---|
| Engine nil (disabled) | Falls through to FTS5, no panic | [ ] |
| Typesense unavailable at search time | Falls through to FTS5, logs warn | [ ] |
| Index non-blocking when channel full | Returns immediately, drops silently | [ ] |
| Backfill 0 events | No-op, no error | [ ] |
| Backfill 250 events | 3 batches (100+100+50), all upserted | [ ] |
| Search with authors filter | filter_by includes pubkey clause | [ ] |
| Search with since/until | filter_by includes created_at clauses | [ ] |
| Search returns IDs → SQLite fetch | Full events returned in relevance order | [ ] |

## Decisions

### 2026-03-26 — Backfill via raw SQL, not store.Query

**Context:** Need to page through all kind 32267 events at startup.
**Options:** A) `store.Query` with paginated filters, B) raw `db.DB.QueryContext` with LIMIT/OFFSET.
**Decision:** Raw SQL (B).
**Rationale:** `store.Query` routes through the custom `queryBuilder` which would call
Typesense — circular. Raw SQL is simpler, avoids the filter/policy stack, and `Store.DB`
is public.

### 2026-03-26 — Platform filter stays in SQLite

**Context:** `filter.Tags["f"]` (platform identifier) is a structured exact-match filter,
not a search concern.
**Options:** A) Store platform in Typesense and filter there, B) let Typesense return IDs,
then SQLite re-filters by platform via `filter.IDs + filter.Tags["f"]`.
**Decision:** B — SQLite re-filters.
**Rationale:** Keeps Typesense schema minimal. Platform is always an exact match, never
a search term. The IDs-then-SQLite pattern already handles all other filter fields.

### 2026-03-26 — FTS5 retained as fallback

**Context:** Typesense is an external process; it can be down or misconfigured.
**Decision:** If `engine.Search()` returns an error, log a warning and fall through to
`appSearchQuery` (FTS5). Relay never returns an error to the client due to Typesense
being unavailable.

## Progress Notes

**2026-03-26:** Work packet written. Starting implementation.
