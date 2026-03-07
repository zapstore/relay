---
description: Architecture — package layout, relay/blossom separation, ACL, analytics, rate limiting
alwaysApply: true
---

# relay — Architecture

## Core Principle

The server is two services in one binary: a Nostr relay and a Blossom CDN server. They share infrastructure (ACL, config, rate limiting) but have separate HTTP handlers and storage.

## Package Layout

```
main.go / cmd/           Entry point, server startup, config loading
pkg/
  relay/                 Nostr relay — WebSocket handler, event validation, NIP-42 auth
  blossom/               Blossom server — upload, download, CDN redirect (Bunny)
  acl/                   Access control — hot-reloadable CSV allow/block lists, Vertex DVM
  indexing/              Demand-driven indexing signals — discovery queue, staleness tracking (shared DB with zindex)
  analytics/             Privacy-preserving impression and download counters (batched SQLite writes)
  rate/                  Token bucket rate limiting per IP group
  config/                Server configuration (env vars via caarlos0/env)
  events/                Event validation and kind-specific structure checks
```

## Data Storage

- `relay.db` — SQLite via `vertex-lab/nostr-sqlite` for Nostr events
- `blossom.db` — SQLite for blob metadata (hash, size, MIME, CDN URL)
- `analytics.db` — SQLite for impression/download counters
- `indexing.db` — SQLite for demand-driven indexing signals (shared with zindex, WAL mode)
- Blobs themselves live on Bunny CDN; server stores metadata only

## Request Flow

### Nostr (WebSocket)
`ws://` → rate limiter → NIP-42 auth check → ACL check → event validation → storage → broadcast

### Blossom Upload
`PUT /upload` → rate limiter → NIP-98 auth → ACL check → dedup check → Bunny upload → metadata store

### Blossom Download
`GET /<hash>` → metadata lookup → CDN redirect (302 to Bunny URL)

## ACL Hot-Reload

CSV files in `acl/` are watched via `fsnotify`. Changes apply without restart.
Unknown pubkey policy (`ALLOW`, `BLOCK`, `VERTEX`) is set via env var.

## Vertex DVM

Reputation-based access for unknown pubkeys. Results cached in LRU. Configured via env vars.
