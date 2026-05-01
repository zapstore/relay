# Zapstore Relay

A Nostr relay and Blossom server for the Zapstore app ecosystem.

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
- Local SQLite metadata store with CDN redirect for downloads

### Access Control in Defender
- Access control is delegated to the Zapstore [defender](https://github.com/zapstore/defender).
- Rate-limiting, cryptographic and structural validation is kept in the relay

### Analytics
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
- A BunnyCDN account with a storage zone configured
- A Nostr secret key loaded with Vertex DVM credits

### Build and Run

```bash
# Clone the repository
git clone https://github.com/zapstore/relay.git
cd relay

# Build with default parameters:
# - TAG = <latest_tag>
# - BUILD_DIR = /build
make

# Or build with specific tag and build directory
make TAG=v1.2.3 BUILD_DIR=path/to/build

# Create and configure .env file
cp .env.example build/.env

# Edit build/.env with your configuration

# Run (use the tag that was built)
./build/relay-v1.2.3
```

### Data Directory Structure

On first run, the server creates the following structure:

```
$SYSTEM_DIRECTORY_PATH/
├── analytics/
│   ├── analytics.db  # SQLite database for analytics
│   └── geo.mmdb      # MaxMind database for ip geolocation
│ 
└── data/
    ├── relay.db      # SQLite database for relay events
    └── blossom.db    # SQLite database for blob metadata
```

### Endpoints

- **Relay**: `ws://localhost:3334` (or your configured port)
- **Blossom**: `http://localhost:3335` (or your configured port)
- **Analytics**: `http://localhost:3336` (or your configured port)

## Analytics API

All endpoints return a JSON array. Date parameters use the `YYYY-MM-DD` format and are inclusive.

### `GET /v1/app/impressions`

Returns aggregated app impression counts recorded from Nostr REQs for kind `32267` app detail views.

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
