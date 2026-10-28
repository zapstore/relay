# Catalog relay

The catalog relay is a Nostr relay, a Blossom file host, and HTTP. The
relay and HTTP are `relay.zapstore.dev`. Blossom is `cdn.zapstore.dev`. It
stores listings, keeps the publisher blacklist, writes snapshots
and bundles, serves `GET /v1/bundle`, answers search, and deletes old APKs.
An app, release, asset, or stack is stored only when it includes
`android-arm64-v8a`. Other `f` tags may be present. An event with no
`android-arm64-v8a` tag is rejected. A kind `30063` on a channel other than
`main` is rejected ([Catalog](../product/specs/catalog.md#listing)).

Device-private events need NIP-13 proof of work at difficulty 20, counted
on the event id: every kind `30078`, and a kind `30267` with content. A
public stack (empty content) and kind `10067` need none. Kind `30078` is
accepted for `zapstore-settings`, `zapstore-device-state`,
`zapstore-device-key-backup`, and `zapstore-update-ignore`.

This document covers what is specific to the relay. [Catalog](../product/specs/catalog.md)
defines listings and `GET /v1/bundle`.
[Publisher policy](../product/specs/publisher-policy.md) defines the gate and
bans. [IPC](../product/specs/ipc.md) is the local socket.

## Publisher gate

The [publisher policy](../product/specs/publisher-policy.md) is the gate.
This process runs it after the blacklist, on listing events (kind `32267`,
`30063`, `3063`) and on Blossom uploads. `RELAY_PUBKEY` is the catalog key.
A key on the whitelist, or one that has already published a kind `32267`,
is not sent to Vertex. A listing event for an app id that already has a
kind `32267` skips the gate; the C1 check still decides the developer slot.
An upload from the active C1 owner or delegate of a certificate on such an
app is accepted, because the upload names no app id. The operator adds a key with `allow` on `relay.sock`
or from the webapp's `/admin`, which sends it there. `disallow` removes it. `allowed` lists the keys.
The web of trust threshold is `VERTEX_THRESHOLD`.

A catalog key listing for a new app id passes the forge check again: the
`repository` tag that key signed must name an approved forge and an account
at least 30 days old. The relay reads the account date from the forge API
once and keeps it. A listing that fails is rejected. The indexer holds a
candidate until its account is old enough, so this only catches an indexer
mistake.

Kinds `1111`, `9735`, and `1984` skip the gate. The relay is the catalog
key's inbox for comments and zaps. Reports are public, like comments. The
operator also reads them through Sentinel.

Kind `1059` is accepted only as an anonymous bug report from the app: one
`p` tag naming the catalog key, NIP-13 difficulty 16 on the id, and at most
64 KiB of content. It is a NIP-17 message gift-wrapped from a one-time key;
the catalog key opens it in any NIP-17 client.

## Blossom

Uploads follow the [publisher policy](../product/specs/publisher-policy.md#uploads).
The pruner runs every
`BLOSSOM_PRUNE_INTERVAL` and deletes every served blob that no current event names,
including pending events, on Bunny and in `blossom/`. It skips the buffer
zone. Zero disables it. An APK is at
most 500 MB and an image at most 500 KB. The limit is the number of bytes
read, not the size hint. Any other media type is refused.

A `GET` for a blob the relay does not serve, or one still in the buffer
zone, is `404`. With `?redirect` it is a temporary redirect to the `url`
on the kind `3063` that names that hash. Flutter 1.1.3 sends that for an
APK whose own URL is not on this host. App v2 does not, and the query
goes when 1.1.3 does.

## Buffer zone

Developer listing events, their files, and a new active kind `30509` wait
in the buffer zone until promotion, as [Catalog](../product/specs/catalog.md#selection)
defines. A catalog-key listing is stored when it arrives. It is not held
for a check the relay does not run on that key. An upload lands on local
disk and is not served until a stored or promoted listing names it.
Promotion serves a developer listing and marks the files to move;
a mover uploads them to Bunny when it is set, then deletes the local copy.
Without Bunny, promoted files stay in `blossom/`. Anything not promoted
within `RELAY_REMOVE_PENDING_AFTER` is deleted, events and files.

Promotion runs when what a listing waits on arrives: its event, the upload
of its APK, the read of its APK, or its processing. An upload or a read
retries only the pending listings whose asset names that file, and the
pending claims for its certificate. Once at start and then every hour, the
relay sweeps the whole buffer zone, for what a restart left pending, and
deletes what expired.

A listing does not need its APK in this relay's Blossom. The relay reads
each developer APK's signer and v3 lineage off the event path, from the
buffer zone or Bunny, else from `$DATA_DIR/apk-cache/<sha256>.apk`, else
from the asset's `url`, checking the sha256. It keeps the result in memory;
a restart reads the file again. Only a pending asset's APK is read: a
developer listing waits until each of those is read, and a stored asset
or release was read when it was promoted and is not checked again. The
catalog key's APK is not read: the indexer verified it, and the relay pins
the `apk_certificate_hash` its asset names. A catalog-key asset is stored
whether or not its file is here; a file with no `url` is served once it is
in Blossom. A file downloaded from a `url`
is kept at `$DATA_DIR/apk-cache/<sha256>.apk`, so the enricher reads it
there. A file that cannot be fetched is fetched again, after 15 seconds
and doubling up to 5 minutes, until it is read or nothing pending names it.
An APK over the size limit is refused and is not fetched again.

An admitted developer release is processed just before it is listed. A
catalog-key release is stored first; processing still runs, and a refusal
deletes it. The relay's
`Processor` runs one release at a time, off the event path. Today that is the
enricher; security checks will join it. An app the enricher could not
finish is still listed, without its artifacts, until an operator runs
`enrich` again. It runs once per release: an error is logged and the
release is listed. A refusal drops the release with its reason. A
release whose every file is already listed, by the same test, is not
processed again. Results are kept in memory by release id.

No store may hold the signing key of a developer APK.
`pkg/relay/signers.txt` lists what shows one: F-Droid's build-key subject
and store certificates, which are refused, and the marks Play adds on
delivery, its source stamp and frosting, which are not: a developer listing
has a C1 proof signed with the certificate key, which shows the developer
holds it. The refusal is `third-party signer: <store>, by its <mark>`. Since the read is remembered, sending the release again is
refused at once. `cmd/signers` reports which stored or local APKs the
rules match.

The indexer writes each APK it verified to `apk-cache` before publishing
it; the relay adds the ones it downloads from a `url`. Nothing prunes it
yet.

A listing kept in the buffer zone is answered `OK` true with a message
that starts `pending:`. Refusals found later are logged; a client learns
them by sending the release again.

A new proof is checked against the certificate of the promoted APK for its
app, else against the buffered APK of the listing it came with. The relay
reads that certificate from the file itself: from the buffer zone, else
from Bunny, else from the asset's `url`. Only the newest listed APK with
that certificate is read. A proof with neither APK is refused. `cert` is
ignored.

## Local client

A `relay.sock` connection whose first byte is `[` speaks NIP-01: one JSON
array per line. It is a client, the same messages as WebSocket. An event
signed by `RELAY_PUBKEY` is stored only from this socket, and a write on
this socket is stored only when the pubkey is `RELAY_PUBKEY`. `REQ` is a
normal read. `zsp` reaches it by putting the socket path in `RELAY_URLS`. Any other
first byte is the control channel.

## Search

The relay ranks its selected listings, as [Search](../product/specs/search.md#relay)
defines, for a NIP-50 REQ on kind `32267`. Vectors and facts come from `artifacts.db`.
The query encoder is the enricher's, loaded at start from `LEAF_MODEL_DIR`
with `ONNXRUNTIME_LIB`; the relay does not start without it. Before it
listens, the relay loads the selected listings and their artifacts for
ranking, filling every missing `listings` row; a failure stops the start.
It reloads them every minute in the background, keeping the previous set
when a reload fails, so a search never waits for a load. A promoted listing
is ranked within a minute, without waiting for a snapshot. Queries are not logged.

## Queries

A REQ reads the promoted event table, then applies [selection](../product/specs/catalog.md#selection)
to kinds `32267`, `30063`, and `3063`: for an app id, it returns only events
signed by the selected listing's pubkey. Only an app with listings from
two pubkeys needs the choice. A filter whose `authors` names the signer keeps that signer's
events, so a publisher can read its own listing. Bundles and search use the
selected catalog only.

Selection is stored on write, not decided per REQ. Table `listings` holds
one row per app id with a kind `32267`: the selected kind `30063`, or NULL
when no listing is complete. Its signer holds the slot. Triggers on `events`
clear an app's row when any of its three kinds is inserted or deleted, by
any writer. The relay fills every app with no row after each event, after
each promotion, on each sweep, after a ban, and at start, reading and writing in one
transaction, so a write that lands in between fails the fill rather than
leaving a stale row. A REQ drops the events of other signers; an app with
no selected release is not filtered. The webapp reads the same table.

`listing:all` and `listing:asset` in `search` are a [live read](../product/specs/catalog.md#live-read):
snapshot events for each selected app, and no ranking. `listing:asset` sends
the kind `32267` and the one latest kind `3063`. The vague-filter check runs
on the filter as it arrived, so either token alone is specific enough. A
query that still has text is a search, and both tokens do nothing.

Comments, zaps, and reports follow [Catalog](../product/specs/catalog.md#comments-zaps-and-reports).
Kinds `1111`, `9735`, and `1984` skip the publisher gate. The relay accepts
them on any kind `32267` coordinate for a listed app id. Kinds `1111` and
`1984` need an `i` tag with that app id; the relay ignores `i` on kind
`9735`. The relay stores a zap whose `p` is a known pubkey
for that app. It does not verify that the receipt matches a paid invoice.

## Counters

The relay counts each finished `GET /v1/bundle` in `analytics.db`, table
`bundle_requests`. SQLite blocks on writes, so the counter does not share
`relay.db`. The handler answers first, then queues the row. One row per
day, split by `from`, `to`, HTTP status, protocol (`v1`), and client
version (query `v`). A missing epoch is `-1`. An absent or unusable client
version is an empty string. No IP is written. A rate limit may see the
address in memory; the `429` is still counted. Later intelligence reads
that table. `GET /v1/bundle/requests` on the analytics API returns the
same rows.

Every phone fetches bundles, wherever its APKs come from, so this table is
the usage signal. Requests per day over the app's polling interval
estimates active phones. `from=0` with `200` is a full catalog: a first
sync, cleared app data, or a `from` with no snapshot. `client_version`
shows version adoption, and `from` against `to` shows how far behind
phones are.

The relay also keeps daily REQ, filter, and saved-event totals in
`relay_metrics`. It does not count APK downloads: most come from the
forge, and Bunny's statistics and log reports cover what the CDN serves,
including the most requested files. Nothing counts app page views on the
relay; phones browse the catalog on the device. No IP and no country are
written.

## Deletion

A kind `5` that deletes the current developer-slot listing,
`32267:<that-pubkey>:<app-id>`, removes every listing for that app id.
The kind `5` stays, and the indexer reads it. The signer is the owner or
delegate who published that listing. A proof
with no self-published listing does not. Any other kind `5` deletes only
the sender's own events, as NIP-09 says. The operator's kind `5`, signed
by the catalog key, deletes anything. See
[Catalog](../product/specs/catalog.md#removal-by-the-developer).

The operator bans over
[IPC](../product/specs/ipc.md#operator). Each ban is stored as one record,
as the [publisher policy](../product/specs/publisher-policy.md#the-ban)
says. The webapp bans page reads those records.

The relay resolves the live `relay.db` into the current catalog, then
stores that result as a snapshot. It does not keep a second event table, and
it does not write one uncompressed directory per epoch.

## Process

The [enricher](../steroid/SPEC.md) writes `about`, `security`, `vector`,
and `icon` in `data/artifacts.db` when a release is admitted, before it is
listed. It does not write a snapshot. A bundle includes the artifacts that
match each listing's current APK.

The relay, on `CATALOG_BUNDLE_INTERVAL` (default five minutes):

1. Loads listing events, kind `30509`, kind `0`, the catalog key's kind
   `30267`, and the catalog key's kind `10067`.
2. Applies [selection](../product/specs/catalog.md#selection).
3. Compares it with the previous snapshot. An unchanged catalog writes nothing.
4. Otherwise writes snapshot `N` and the signed bundle `(N-1)-N`.

`CATALOG_BUNDLE_INTERVAL=0` disables the tick. `relay bundle` writes one snapshot and exits. `relay bundle <app-id>` enriches only the listings whose id contains that string; the snapshot is still the whole catalog, so a filter never removes an app from phones. A filter that matches nothing is an error and writes nothing. The tick takes no filter.

One seal runs at a time per data directory, across processes: `relay bundle` and a running relay take `seal.lock`, so an epoch is never written twice.

`READ_ONLY=true` rejects events on the WebSocket and on `relay.sock`, Blossom uploads, and `relay.sock` bans and whitelist changes. Queries, downloads, and `GET /v1/bundle` stay. The bundle tick and `relay bundle` still sign with `SIGN_WITH`; `relay bundle <app-id>` still enriches.

`SIGN_WITH` is required. It is `1`, 64-character hex, `nsec`, or a `bunker://`
URL from hotbox. Its public key must equal `RELAY_PUBKEY`. Hex and `nsec`
are for someone else running this relay. The servers we operate set a
hotbox `bunker://` URL, as [Infra](../infra/SPEC.md#secrets) says.

## Storage

```
$DATA_DIR/
  snapshots/1.tar.zst          # full resolved catalog at epoch 1
  snapshots/2.tar.zst
  bundles/0-1.tar.zst          # signed bundle
  bundles/1-2.tar.zst
  epoch-floor                  # highest epoch written, kept across a rebuild
  seal.lock                    # held while a snapshot is written
  artifacts.db                 # about, security, vector, icon, publisher pictures
```

Latest epoch is the largest `N` in `snapshots/N.tar.zst`. These paths are not
configurable. The next snapshot is one above the larger of that and
`epoch-floor`, so clearing `snapshots/` and `bundles/` for a rebuild never
reuses an epoch. A request whose `from` has no snapshot gets the full
snapshot, `from=0`, with `200`.

A snapshot is the full event set at that epoch, and the ban notices in
force when it was written. A bundle built later from it carries those ban
lines, not the ones in force at request time. A bundle is the difference
from one epoch to another: `diff.jsonl`, the artifacts for changed apps,
and avatars whose picture URL changed. An app's artifacts are included only
when the enricher built them for that listing's current APK hash.

`diff.jsonl` is defined in the
[catalog](../product/specs/catalog.md#members). A ban line is written
only for an app id that is in the delete list and has operator text.

## `GET /v1/bundle`

`to` is the latest snapshot when the request starts. The relay serves
`bundles/<from>-<to>.tar.zst`, building it from snapshot `from` and
snapshot `to` when the file is not there yet. `from=0` is built from
snapshot `to` alone and cached the same way.
