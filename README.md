# Relay

The Relay is an app store relay designed for Zapstore. It keeps software events and indexes repositories from GitHub. Also, this relay acts as a community (NIP-CC) relay for Zapstore premium users.

## How to run?

You have to set environment variables defined in [the example file](./.env.example) on a `.env` file with no prefixes in the same directory with executable. 

**Important Environment Variables**:
- `RELAY_PORT`: Port for the Nostr relay (default: `:7777`)
- `HTTP_PORT`: Port for the HTTP API endpoints (default: `:8080`)

Then you can build the project using:

```sh
go build -tags "sqlite_fts5" .
```

> `make build` will do the same for you.

Then you can run the relay using:

```sh
./relay
```

The relay will start two servers:
- Nostr relay server on the configured `RELAY_PORT`
- HTTP API server on the configured `HTTP_PORT`

**Example API Usage**:
```bash
# Check if a pubkey is blacklisted
curl "http://localhost:8080/api/v1/blacklist?pubkey=npub1example..."

# Get WoT rank for a pubkey
curl "http://localhost:8080/api/v1/wot-rank?pubkey=npub1example..."
```

## HTTP API

The relay also provides HTTP API endpoints for checking blacklist status and Web of Trust (WoT) rankings:

### Endpoints

#### 1. Check Blacklist Status
- **Endpoint**: `GET /api/v1/blacklist?pubkey={pubkey}`
- **Description**: Check if a public key is blacklisted
- **Parameters**:
  - `pubkey` (required): The public key to check

**Success Response** (200 OK):
```json
{
  "success": true,
  "data": {
    "pubkey": "npub...",
    "is_blacklisted": false
  }
}
```

**Error Response** (400/500):
```json
{
  "success": false,
  "error": "error message"
}
```

#### 2. Get WoT Rank
- **Endpoint**: `GET /api/v1/wot-rank?pubkey={pubkey}`
- **Description**: Get the Web of Trust rank for a public key
- **Parameters**:
  - `pubkey` (required): The public key to get rank for

**Success Response** (200 OK):
```json
{
  "success": true,
  "data": {
    "pubkey": "npub...",
    "rank": 0.75
  }
}
```

**Error Response** (400/500):
```json
{
  "success": false,
  "error": "error message"
}
```

# License

[MIT License](./LICENSE)
