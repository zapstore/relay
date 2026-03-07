---
description: Product vision — what the relay is, who it serves, what success means
alwaysApply: true
---

# relay — Vision

## What the Server Is

The Zapstore relay is the infrastructure backbone: a Nostr relay and Blossom CDN server purpose-built for the Zapstore app ecosystem. It stores and serves NIP-82 software events and APK/media blobs.

## Who Uses It

- `zsp` and `zindex` publish events and upload blobs to it
- `webapp`, `zapstore` (Flutter), and `zapstore-cli` read events and download blobs from it
- Developers querying `wss://relay.zapstore.dev` directly

## What Success Means

- Events are stored reliably and served with low latency
- Bad actors (spam, abuse) are blocked without affecting legitimate publishers
- Blob downloads are fast via CDN redirect — the server is never a bandwidth bottleneck
- The server runs unattended; ACL changes apply without restarts

## Non-Goals

- The relay is not a general-purpose Nostr relay (only Zapstore-relevant kinds)
- The relay does not store blob content locally (CDN only)
- The relay does not perform app indexing (that's `zindex`)
- The relay does not have a web UI
