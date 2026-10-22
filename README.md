# flixer-indexer

Go service that scans flixer's media libraries, identifies items, fetches metadata and artwork, probes media files, and saves results back to flixer.

## Pipeline

`scan:library` → `identify:item` → `metadata:fetch` → `images:fetch` → `probe:item` (finalizes and upserts to flixer). `refresh:item` re-processes a single path.

Each stage is an Asynq queue (Redis) with its own worker pool; concurrency can be changed at runtime from flixer.

- Local NFO files and local artwork win; remote metadata/images only fill gaps.
- Providers: TMDB, TVDB, OMDb, Fanart.tv, AniList, AniDB (aid only), MusicBrainz/Cover Art Archive, TheAudioDB, Discogs.
- Artwork is stored alongside media or in `CACHE_PATH`, per library setting.

## Running

```
go run ./cmd/flixer-indexer
```

Requires Redis, and `ffprobe`/`ffmpeg` on PATH (or `FFPROBE_PATH`/`FFMPEG_PATH`). See [.env.example](.env.example) for all variables.

| Variable | Default | Notes |
| --- | --- | --- |
| `PORT` | 8092 | |
| `ROLE` | all | `all`, `api` or `worker` |
| `FLIXER_URL` | http://localhost:8083 | alias `FLIXER_INTERNAL_URL` |
| `STREAM_TOKEN_SECRET` | — | shared HMAC secret (falls back to `JWT_SECRET`) |
| `REDIS_ADDR` | localhost:6379 | |
| `CACHE_PATH` | ./cache/metadata | |
| `WATCH_FOLDERS` | true | watch library folders and auto-scan after changes settle |
| `WATCH_DEBOUNCE` | 30s | quiet time before an auto-scan is queued |
| `WATCH_RESCAN_INTERVAL` | 0 (off) | periodic scan for SMB/NFS mounts that emit no file events, e.g. `15m` |
| `FLIXER_MODE` | — | `stub` reads `STUB_LIBRARIES_FILE` (`{"libraries":[...]}`) and appends items to `STUB_OUTPUT_FILE` |

Provider keys (`TMDB_API_KEY`, `TVDB_API_KEY`, `OMDB_API_KEY`, `FANART_API_KEY`, `AUDIODB_API_KEY`, `DISCOGS_TOKEN`, `ANIDB_CLIENT`) are fallbacks; flixer's settings take precedence.

## Docker

```yaml
services:
  redis:
    image: redis:7-alpine
  flixer-indexer:
    build: ../flixer-indexer
    environment:
      FLIXER_URL: http://flixer:8083
      STREAM_TOKEN_SECRET: ${STREAM_TOKEN_SECRET}
      REDIS_ADDR: redis:6379
    volumes:
      - ${STREAM_MEDIA_ROOT}:/media
      - indexer-cache:/var/cache/flixer/indexer
    depends_on: [redis]
volumes:
  indexer-cache:
```

## Auth

Requests in both directions carry `X-Indexer-Timestamp` and `X-Indexer-Signature` = hex HMAC-SHA256 of
`flixer-indexer:v1:<ts>:<METHOD>:<path>:<sha256(body)>` with `STREAM_TOKEN_SECRET` (±5 min skew). `/health` is open.

## flixer contract (to be implemented in flixer)

- `GET /api/indexer/libraries` → `{libraries:[...]}`
- `GET /api/indexer/settings` → `{providers, concurrency, language}`
- `POST /api/indexer/media/items` → `{id}` (4xx other than 429 is treated as permanent)

Hierarchy and identity: every item has `externalKey` (e.g. `tmdb:100:s1:e2`, or `path:<path>` if unidentified) and `parentExternalKey` (series → season → episode). Upsert by `externalKey`, link parents through it (creating a placeholder parent if it hasn't arrived yet), and fall back to `parentProviderIds` / `parentPath` to match a series identified under a different provider. A renamed folder then keeps the same row.

## Internal API

| Endpoint | Purpose |
| --- | --- |
| `POST /internal/scan`, `POST /internal/refresh` | start a library scan / refresh a path |
| `GET /internal/scans`, `GET /internal/stats` | progress and stats |
| `GET /internal/tasks?queue=&state=&page=&size=` | list tasks |
| `POST /internal/tasks/{queue}/{id}/cancel`, `/retry` | task control |
| `POST /internal/queues/{queue}/pause`, `/resume` | queue control |
| `GET`/`POST /internal/concurrency` | worker counts |
| `POST /internal/settings/reload` | re-fetch settings from flixer |