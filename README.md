# relay

The Zapstore catalog relay: a Nostr relay, a Blossom file host, and `GET /v1/bundle` over HTTP.

## Features

- **Nostr relay** on [rely](https://github.com/pippellia-btc/rely), with [NIP-11](https://github.com/nostr-protocol/nips/blob/master/11.md) and [NIP-42](https://github.com/nostr-protocol/nips/blob/master/42.md). It accepts only the configured event kinds and checks each one's structure. It rejects filters that are too vague. Events are stored in SQLite.
- **Blossom** on [blossy](https://github.com/pippellia-btc/blossy), for APKs and images. Blobs are checked before upload, so a duplicate is not sent again. Uploads stay on local disk, unserved, until a promoted listing names them. A mover then sends them to [Bunny CDN](https://bunny.net/), and downloads redirect there. Without Bunny, files are served from local disk.
- **Publisher policy.** The gate and bans are the [publisher policy](../product/specs/publisher-policy.md).
- **Rate limiting.** A token bucket per IP group, with a cost per operation (connections, events, queries, uploads) and a penalty for misbehaving clients.

## Catalog

- `GET /v1/bundle` is on the relay listener.
- A snapshot is written on `CATALOG_BUNDLE_INTERVAL` (default 5 minutes). The tick calls the enricher for apps in the diff first. `CATALOG_BUNDLE_INTERVAL=0` disables that tick. `relay bundle` enriches that diff and writes one snapshot, then exits. `relay bundle <app-id>` enriches only listings whose id contains that string; the snapshot is still the whole catalog. The tick takes no filter.
- Signing is `SIGN_WITH`. Its public key is `RELAY_PUBKEY`: unset, `RELAY_PUBKEY` takes it; set, it must equal it.
- `READ_ONLY=true` rejects events, Blossom uploads, and `relay.sock` blacklist and whitelist changes. Bundles are still signed with `SIGN_WITH`. The bundle tick still enriches.
- `BLOSSOM_PRUNE_INTERVAL` deletes APKs that no current asset event names, on Bunny and in `DATA_DIR/blossom`. Required. Zero disables it.

## relay.sock

- The [IPC](../product/specs/ipc.md) socket. `RELAY_SOCKET_PATH` sets its path; the default is `relay.sock` in the data directory. Mode 0660.
- A connection that opens with `[` speaks NIP-01: one JSON array per line, closed after 30 seconds. It is the only place the catalog key's events are accepted, and it skips IP rate limits.
- Any other connection is one JSON command. Reads (`allowed`, `jobs`) need only access to the socket file. A write needs the relay's own Unix user, checked with the peer credentials, or a command event signed by a key in `ADMIN_PUBKEYS`.
- Every write is logged to the relay log.

## Analytics

- Off unless `ANALYTICS_ADDRESS` is set. Unset means no database, no collection, and no API.
- Daily counts of bundle requests and relay REQs and events. No IP, country, or client header is written.
- APK downloads are not counted; the Bunny dashboard covers what the CDN serves.
- Writes are queued in memory and flushed to SQLite periodically or when the batch is full, so they never block a request.

## Build

Needs Go 1.25 or later, and zig to cross-compile.

```bash
make build
make linux-arm64
make linux-amd64
```

The binary is `dist/relay-<os>-<arch>`. `make build` also links it as `./relay`. `make help` lists every target.

## Run

```bash
cp .env.example .env
./relay run
./relay bundle
./relay bundle org.mozilla
```

Production needs a BunnyCDN storage zone. The publisher gate needs a Nostr secret key with Vertex DVM credits.

## Configuration

Every variable, its default, and its unit are in [`.env.example`](.env.example). `DATA_DIR`, `SIGN_WITH`, `ADMIN_PUBKEYS`, `BLOSSOM_PRUNE_INTERVAL`, and the three hostnames are required. Old names are not read.

## Data directory

On first run, the relay creates:

```
$DATA_DIR/
├── relay.db      # events, the blacklist, and the whitelist
├── blossom.db    # blob metadata
├── artifacts.db  # about, security, vector, icon, and publisher pictures
├── analytics.db  # only when ANALYTICS_ADDRESS is set
├── blossom/      # blob bytes in the Bunny layout: the buffer zone, or every blob when Bunny is unset
├── snapshots/    # catalog snapshots, one per epoch
├── bundles/      # signed GET /v1/bundle bodies
├── cache/        # the enricher's source trees
├── models/       # the search and enrichment model, downloaded on first use
└── relay.sock    # unless RELAY_SOCKET_PATH is set
```

## Endpoints

- **Relay** and `GET /v1/bundle`: `http://localhost:3334` (or your configured port)
- **Blossom**: `http://localhost:3335` (or your configured port)
- **Analytics**: off unless `ANALYTICS_ADDRESS` is set (for example `localhost:3336`)

## Analytics API

All endpoints return a JSON array. Date parameters use the `YYYY-MM-DD` format and are inclusive.

### `GET /v1/metrics/relay`

Returns daily relay traffic metrics (REQ count, filter count, event count).

| Parameter | Type | Description |
|-----------|------|-------------|
| `from` | date | Start of date range (inclusive) |
| `to` | date | End of date range (inclusive) |

### `GET /v1/bundle/requests`

Returns aggregated `GET /v1/bundle` responses. `from_epoch` and `to_epoch` are catalog epochs. `-1` means that epoch was not established. `client_version` is the Zapstore version from the `v` query.

| Parameter | Type | Description |
|-----------|------|-------------|
| `from` | date | Start of date range (inclusive) |
| `to` | date | End of date range (inclusive) |
| `from_epoch` | integer | Filter to a source epoch, including `0` and `-1` |
| `to_epoch` | integer | Filter to a target epoch, including `0` and `-1` |
| `status` | integer | Filter to an HTTP status |
| `protocol` | string | Filter to a path version, such as `v1` |
| `client_version` | string | Filter to a Zapstore version |
| `group_by` | CSV | Grouping dimensions: `day`, `from_epoch`, `to_epoch`, `status`, `protocol`, `client_version` |
