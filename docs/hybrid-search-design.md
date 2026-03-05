# Hybrid Search Design

## Overview

Combine the existing FTS5/BM25 keyword search with vector similarity search
to support natural language app queries. No LLM, no query router — hybrid
scoring handles the fallback naturally.

## Architecture

```
Query
  │
  ├──► FTS5 (BM25)         ── keyword/trigram matching
  │
  ├──► sqlite-vec (cosine)  ── semantic similarity
  │
  └──► Hybrid merge
         │
         score = α × norm(bm25) + (1 - α) × cosine_sim
         │
         ▼
       Ranked results
```

## Components

### Embedding model

`all-MiniLM-L6-v2` — 384-dimensional vectors, ~80 MB.
Run via ONNX Runtime in-process or as a local HTTP embedding server.

### Vector table (sqlite-vec)

```sql
CREATE VIRTUAL TABLE IF NOT EXISTS apps_vec USING vec0(
    id TEXT PRIMARY KEY,
    embedding float[384]
);
```

Loaded as a SQLite extension alongside FTS5.

### Embedding pipeline

On every KindApp (32267) insert, concatenate `name + summary + content`,
compute the embedding, and upsert into `apps_vec`.

A one-time migration job embeds all existing apps.

### Hybrid query

```sql
WITH fts_results AS (
    SELECT fts.id, bm25(apps_fts, 0, 20, 5, 1) AS score
    FROM apps_fts fts
    WHERE apps_fts MATCH ?
    LIMIT 50
),
vec_results AS (
    SELECT id, distance
    FROM apps_vec
    WHERE embedding MATCH ?
    ORDER BY distance
    LIMIT 50
)
SELECT COALESCE(f.id, v.id) AS id,
       COALESCE(f.score, 0) * :alpha + COALESCE(1.0 - v.distance, 0) * :beta AS combined
FROM fts_results f
FULL OUTER JOIN vec_results v ON f.id = v.id
ORDER BY combined DESC
LIMIT ?;
```

Existing tag filters (`#f`, `#t`, authors, date range) apply as additional
WHERE clauses on a final JOIN to the `events` table, same as today.

### Tuning

`α` controls the BM25/vector balance. Start at 0.4 and tune empirically.

- Short keyword queries (e.g. `"signal"`) — BM25 dominates via exact match.
- Natural language queries (e.g. `"privacy focused messenger"`) — cosine
  similarity dominates when exact words don't appear in metadata.
