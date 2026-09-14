# spoonfeed

Small stateless ATProto feed server for a local/test PDS.

## Design

- Reads `/pds/actors/*/did:*/store.sqlite` directly.
- Opens PDS SQLite databases read-only.
- No firehose/Jetstream consumer.
- No persistent index.
- Builds a complete in-memory index and atomically swaps it in.
- Rebuilds periodically.
- Counts likes across all actor repositories.
- Counts direct replies across all actor repositories.
- Ranks posts with:

    points = likes + replies*2
    score = max(points-1, 0) / (age_hours+2)^1.8

## Configuration

Configuration can be set via `.env` file or command line flags (flags override `.env`). Copy `.env.example` to `.env` and adjust the values:

```bash
cp .env.example .env
```

Available configuration variables:
- `FEEDGEN_LISTEN`: HTTP listen address (default: `:4040`)
- `FEEDGEN_HOSTNAME`: Public hostname for the feed generator (default: `spoonfeed.devsky.app`)
- `FEEDGEN_SERVICE_DID`: Service DID for `did:web` resolution (default: `did:web:spoonfeed.devsky.app`)
- `FEEDGEN_ACTORS_PATH`: Path to PDS actors SQLite databases (default: `/pds/actors`)
- `FEEDGEN_REFRESH`: Index refresh interval (default: `30s`)
- `FEEDGEN_MAX_AGE`: Maximum age of indexed posts (default: `720h` / 30 days)
- `FEEDGEN_FEED_URI`: AT URI of the feed generator record (default: `at://did:plc:nuzd73csefxfqwnuprvomqbp/app.bsky.feed.generator/spoonfeed`)
- `FEEDGEN_PUBLISHER_HANDLE`: Operator/Publisher handle (e.g. `operator.devsky.app`)
- `FEEDGEN_PUBLISHER_DID`: Operator/Publisher DID (e.g. `did:plc:nuzd73csefxfqwnuprvomqbp`)
- `FEEDGEN_PUBLISHER_APP_PASSWORD`: Operator app password (for feed registration scripts)

## Build

```bash
go mod tidy
go build -o spoonfeed .
```

## Run

Run directly with `.env` configured:

```bash
./spoonfeed
```

Or pass flags to override settings:

```bash
./spoonfeed \
  -actors /pds/actors \
  -listen :4040 \
  -refresh 30s \
  -hostname spoonfeed.devsky.app \
  -feed-uri 'at://did:plc:nuzd73csefxfqwnuprvomqbp/app.bsky.feed.generator/spoonfeed'
```

## Endpoints

- `GET /.well-known/did.json` - DID document for `did:web` resolution.
- `GET /xrpc/app.bsky.feed.describeFeedGenerator` - Feed generator metadata and supported feeds.
- `GET /xrpc/app.bsky.feed.getFeedSkeleton?feed=<URI>&limit=<N>&cursor=<C>` - Feed skeleton containing post AT URIs.
- `GET /healthz` - Health check and post index count.

## Test

```bash
# Check DID document
curl http://127.0.0.1:4040/.well-known/did.json

# Check Health
curl http://127.0.0.1:4040/healthz

# Describe feed generator
curl http://127.0.0.1:4040/xrpc/app.bsky.feed.describeFeedGenerator

# Get feed skeleton
curl 'http://127.0.0.1:4040/xrpc/app.bsky.feed.getFeedSkeleton?feed=at%3A%2F%2Fdid%3Aplc%3Anuzd73csefxfqwnuprvomqbp%2Fapp.bsky.feed.generator%2Fspoonfeed&limit=10'
```

The skeleton response contains post AT URIs. The Bluesky app/AppView resolves those into the actual posts.

## Important behavior

The cursor is an in-memory ranking offset encoded as base64. A full refresh can change ranking order, so cursors should be considered short-lived. This is fine for the initial test implementation and can be replaced with a stable score/URI cursor later.
