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
# Check if a pubkey is accepted to publish software events
curl "http://localhost::3335/api/v1/accept?pubkey=npub1example..."
```

# License

[MIT License](./LICENSE)
