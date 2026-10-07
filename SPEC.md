# Catalog relay

The catalog relay is a Nostr relay, a Blossom file host, and HTTP. The
relay and HTTP are `relay.zapstore.dev`. Blossom is `cdn.zapstore.dev`. It
stores listings, keeps the publisher blacklist, writes snapshots
and bundles, serves `GET /bundle` and `POST /suggest`, and deletes old APKs.

This document covers what is specific to the relay. [Catalog](../product/specs/catalog.md)
defines listings, `GET /bundle`, and `POST /suggest`.
[Publisher policy](../product/specs/publisher-policy.md) defines the gate and
bans. [IPC](../product/specs/ipc.md) is the local socket.

## Publisher gate

The [publisher policy](../product/specs/publisher-policy.md) is the gate.
This process runs it after the blacklist, on listing events (kind `32267`,
`30063`, `3063`) and on Blossom uploads. `RELAY_PUBKEY` is the catalog key.
The paid list is the `allowed` table. The web of trust threshold is
`VERTEX_THRESHOLD`. A passing developer admission is a row in `admissions`.

Kinds `1111`, `9735`, and `1984` skip the gate. The relay is the catalog
key's inbox for comments and zaps. Reports are stored for the operator to
read, including through Sentinel.

## Blossom

Uploads follow the [publisher policy](../product/specs/publisher-policy.md#uploads).
The pruner runs every
`PRUNE_INTERVAL` and deletes every blob that no current event names, on
Bunny and in `data/blobs/`. Zero disables it.

## Counters

The relay counts `GET /bundle` in `analytics.db`. SQLite blocks on writes,
so a counter write does not share `relay.db`. It writes only after it has
answered `200` or `304`: one row per day, split by status, including the
`from` epoch. No IP is written. A rate limit may see the address in memory
and drop the request before the count. There is no per-app download count,
and no split by country, client source, or install versus update. Later
intelligence reads that table.

## Deletion

A kind `5` whose `a` tag is `32267:<catalog-pubkey>:<app-id>`, from the
owner or an active delegate of a current kind `30509` proof for that app's certificate,
removes every listing for that app id and records the removal for the
indexer on [IPC](../product/specs/ipc.md#removals). Any other kind `5` deletes only the sender's own events, as
NIP-09 says. The operator's kind `5`, signed by the catalog key, deletes
anything. See [Catalog](../product/specs/catalog.md#removal-by-the-developer).

The operator adds and removes paid publishers and bans over
[IPC](../product/specs/ipc.md#operator). Each ban is stored as one record,
as the [publisher policy](../product/specs/publisher-policy.md#the-ban)
says. The webapp bans page reads those records.

The relay resolves the live `relay.db` into the current catalog, then
stores that result as a snapshot. It does not keep a second event table, and
it does not write one uncompressed directory per epoch.

## Process

The [enricher](../steroid/SPEC.md) writes `<app-id>/about`,
`<app-id>/security`, `<app-id>/vector`, and `<app-id>/icon.webp` under
`data/artifacts/`. It does not write a snapshot.

The relay, on `BUNDLE_INTERVAL` (default five minutes):

1. Loads listing events, kind `30509`, kind `0`, the catalog key's kind
   `30267`, and the catalog key's kind `10067`.
2. Applies [selection](../product/specs/catalog.md#selection).
3. Compares it with the previous snapshot. An unchanged catalog writes nothing.
4. Otherwise calls the enricher for the apps in that diff. A cache that
   already matches the listing is left untouched. An enricher failure is
   logged and the snapshot is still written.
5. Writes snapshot `N` and the signed bundle `(N-1)-N`.

`BUNDLE_INTERVAL=0` disables the tick. `relay bundle` writes one snapshot and exits.

`SIGN_WITH` is required. It is `1`, 64-character hex, `nsec`, or a `bunker://`
URL from hotbox. Its public key must equal `RELAY_PUBKEY`. Hex and `nsec`
are for someone else running this relay. The servers we operate set a
hotbox `bunker://` URL, as [Infra](../infra/SPEC.md#secrets) says.

## Storage

```
$SYSTEM_DIRECTORY_PATH/data/
  snapshots/1.tar.zst          # full resolved catalog at epoch 1
  snapshots/2.tar.zst
  bundles/0-1.tar.zst          # signed bundle
  bundles/1-2.tar.zst
  artifacts/<app-id>/about
  artifacts/<app-id>/security
  artifacts/<app-id>/vector
  artifacts/<app-id>/icon.webp
  artifacts/<64 hex>.webp
```

Latest epoch is the largest `N` in `snapshots/N.tar.zst`. These paths are not
configurable.

A snapshot is the full event set at that epoch. A bundle is the difference
from one epoch to another: `diff.jsonl`, the artifacts for changed apps,
and avatars whose picture URL changed. An app's artifacts are included only
when the enricher built them for that listing's current APK hash.

`diff.jsonl` is defined in the
[catalog](../product/specs/catalog.md#members). A ban line is written
only for an app id that is in the delete list and has operator text.

## `GET /bundle`

`to` is the latest snapshot when the request starts. The relay serves
`bundles/<from>-<to>.tar.zst`, building it from snapshot `from` and
snapshot `to` when the file is not there yet. `from=0` is built from
snapshot `to` alone and cached the same way.
