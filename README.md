# Zapstore relay

The catalog relay: a Nostr relay, a Blossom file host, and HTTP for `GET /bundle`.

## Features

### Nostr Relay
- Full [Nostr](https://github.com/nostr-protocol/nostr) relay implementation using [rely](https://github.com/pippellia-btc/rely)
- [NIP-11](https://github.com/nostr-protocol/nips/blob/master/11.md) relay information document
- [NIP-42](https://github.com/nostr-protocol/nips/blob/master/42.md) authentication support
- Configurable allowed event kinds with structure validation
- Filter specificity scoring to reject overly vague queries
- SQLite-based event storage

### Blossom Server
- Full [Blossom](https://github.com/hzrd149/blossom) server implementation using [blossy](https://github.com/pippellia-btc/blossy)
- [Bunny CDN](https://bunny.net/) integration for scalable blob delivery
- Configurable allowed media types (APKs, images)
- Deduplication: blobs are checked before upload to save bandwidth
- Local SQLite metadata store with CDN redirect for downloads, or local files when Bunny is unset
- Uploads stay on local disk, unserved, until a promoted listing names them. A mover then sends them to Bunny

### Publisher policy
- The gate and bans are the [publisher policy](../product/specs/publisher-policy.md)

### Catalog
- `GET /bundle` is on the relay listener
- A snapshot is written on `BUNDLE_INTERVAL` (default 5 minutes). The tick calls the enricher for apps in the diff first. `BUNDLE_INTERVAL=0` disables that tick. `relay bundle` enriches that diff and writes one snapshot, then exits. `relay bundle <app-id>` keeps listings whose id contains that string, drops the other app events, and passes the same string to the enricher. The tick takes no filter
- Signing is `SIGN_WITH`. Its public key must equal `RELAY_PUBKEY`
- `READ_ONLY=true` rejects events, Blossom uploads, `relay.sock` bans, and dashboard blacklist changes. Bundles are still signed with `SIGN_WITH`. The bundle tick still enriches
- `PRUNE_INTERVAL` (default 24 hours) deletes APKs that no current asset event names, on Bunny and in the local blob directory. Zero disables it

### Analytics
- Off unless `ANALYTICS_API_ADDRESS` is set. Unset means no database, no collection, and no API
- Privacy-preserving usage statistics for app impressions and blob downloads
- Counts impressions derived from Nostr REQs
- Counts downloads from blossom downloads
- Batched, non-blocking writes: events are queued in memory and flushed to SQLite periodically or when the batch size threshold is reached

### Rate Limiting
- Token bucket rate limiting per IP group
- Configurable initial tokens, max tokens, and refill rate
- Different costs for different operations (connections, events, queries, uploads)
- Penalty system for misbehaving clients

## Running

### Prerequisites

- Go 1.25 or later
- For production: a BunnyCDN account with a storage zone configured
- Optional: a Nostr secret key loaded with Vertex DVM credits, for the publisher gate

### Build and Run

```bash
make
make linux-arm64
make linux-amd64
```

The binary is `dist/relay-<os>-<arch>`.

```bash
cp .env.example .env
./dist/relay-$(go env GOOS)-$(go env GOARCH) run
./dist/relay-$(go env GOOS)-$(go env GOARCH) bundle
./dist/relay-$(go env GOOS)-$(go env GOARCH) bundle org.mozilla
```

### Data Directory Structure

On first run, the relay creates the following structure:

```
$SYSTEM_DIRECTORY_PATH/
├── analytics/        # only when ANALYTICS_API_ADDRESS is set
│   ├── analytics.db  # SQLite database for analytics
│   └── geo.mmdb      # MaxMind database for ip geolocation
│ 
└── data/
    ├── relay.db      # SQLite database for relay events and the blacklist
    ├── blossom.db    # SQLite database for blob metadata
    ├── blobs/        # Blob bytes when Bunny is unset
    ├── snapshots/    # Catalog snapshots, one per epoch
    ├── bundles/      # Signed GET /bundle bodies
    └── artifacts.db  # about, security, vector, icon, and publisher pictures
```

### Endpoints

- **Relay** and `GET /bundle`: `http://localhost:3334` (or your configured port)
- **Blossom**: `http://localhost:3335` (or your configured port)
- **Analytics**: off unless `ANALYTICS_API_ADDRESS` is set (for example `localhost:3336`)

## Analytics API

All endpoints return a JSON array. Date parameters use the `YYYY-MM-DD` format and are inclusive.

### `GET /v1/app/impressions`

Returns aggregated app impression counts recorded from Nostr REQs for kind `32267` app page views.

| Parameter | Type | Description |
|-----------|------|-------------|
| `app_id` | string | Filter to a specific app identifier |
| `app_pubkey` | string | Filter to a specific publisher pubkey |
| `from` | date | Start of date range (inclusive) |
| `to` | date | End of date range (inclusive) |
| `source` | string | Filter by client source: `app`, `web`, or `unknown` |
| `type` | string | Filter by impression type: `detail` |
| `group_by` | CSV | Grouping dimensions: `app_id`, `app_pubkey`, `app_version`, `day`, `source`, `type`, `country_code` |

### `GET /v1/app/downloads`

Returns aggregated app download counts recorded from Blossom requests for kind `3063` asset blobs.

| Parameter | Type | Description |
|-----------|------|-------------|
| `hash` | string | Filter to a specific blob hash |
| `app_id` | string | Filter to a specific app identifier |
| `app_pubkey` | string | Filter to a specific publisher pubkey |
| `from` | date | Start of date range (inclusive) |
| `to` | date | End of date range (inclusive) |
| `source` | string | Filter by client source: `app`, `web`, or `unknown` |
| `type` | string | Filter by download type: `install` or `update` |
| `group_by` | CSV | Grouping dimensions: `hash`, `app_id`, `app_version`, `app_pubkey`, `day`, `source`, `type`, `country_code` |

### `GET /v1/metrics/relay`

Returns daily relay traffic metrics (REQ count, filter count, event count).

| Parameter | Type | Description |
|-----------|------|-------------|
| `from` | date | Start of date range (inclusive) |
| `to` | date | End of date range (inclusive) |

### `GET /v1/metrics/blossom`

Returns daily Blossom server traffic metrics (check, download, and upload counts).

| Parameter | Type | Description |
|-----------|------|-------------|
| `from` | date | Start of date range (inclusive) |
| `to` | date | End of date range (inclusive) |
