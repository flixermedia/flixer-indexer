# flixer-indexer

**Specification for building the Flixer media indexing service**

This document is a complete, self-contained specification. An AI (or human) should be able to implement `flixer-indexer` from this file alone, using the existing Flixer repositories as context.

---

## 1. Project Context

### Existing Flixer Ecosystem

| Repo | Language | Role |
|------|----------|------|
| [flixermedia/flixer](https://github.com/flixermedia/flixer) | TypeScript (Ts.ED + Fastify + TypeORM) | Core backend API. Owns PostgreSQL database, settings, auth, and all media persistence. Currently uses **pg-boss** for jobs. |
| [flixermedia/flixer-web](https://github.com/flixermedia/flixer-web) | Vue 3 + Vite + Pinia + Tailwind | Web frontend + future admin dashboard. |
| [flixermedia/flixer-streamer](https://github.com/flixermedia/flixer-streamer) | Go | Dedicated streaming microservice (Direct Play / Stream / FFmpeg). JWT/HMAC auth, range requests. |

- PostgreSQL is the single source of truth.
- Media path is mounted read-only into containers.
- Docker Compose brings the whole stack up behind a reverse proxy.
- Settings live in a global `settings` table (JSON values).
- The media scanner is still on the roadmap — this project implements it.

### Design Decisions (locked)

- **Language**: Go (matches streamer, first-class Asynq support, excellent concurrency for tens of thousands of items).
- **Queue**: Redis + [Asynq](https://github.com/hibiken/asynq).
- **Database ownership**: flixer owns every write. The indexer **never** writes to Postgres directly. It collects metadata and calls flixer’s internal API to persist.
- **Libraries**: Stored in the database (path + type). Indexer discovers library locations from flixer and scans only those paths.
- **Metadata providers**: Everything supported by Jellyfin **and** Plex.
- **Image storage**: Per-library setting — either “alongside media files” or `flixer-data/cache/metadata/...`. Choice is stored in the DB.
- **Concurrency**: Expect tens of thousands of items. Max concurrent workers per queue is adjustable live from flixer-web admin.
- **Image work**: Download + any manipulation runs on a **dedicated** Asynq queue/workers so it never blocks metadata work.
- **NFO / local priority**: Identical to Jellyfin — local NFO files and local images always win over remote providers.
- **Schema**: Green-field for media entities. flixer will expose the internal write API; the indexer only consumes it.

---

## 2. High-Level Architecture

```
┌─────────────────┐     internal API      ┌──────────────────────┐
│  flixer (TS)    │◄─────────────────────►│  flixer-indexer (Go) │
│  - owns DB      │   (add/update media)  │  - Asynq client+srv  │
│  - settings     │                       │  - workers           │
│  - admin proxy  │                       │  - metadata fetchers │
└────────┬────────┘                       └──────────┬───────────┘
         │                                           │
         │                                           ▼
         │                                    ┌─────────────┐
         │                                    │    Redis    │
         │                                    │  (Asynq)    │
         │                                    └─────────────┘
         │
         ▼
┌─────────────────┐
│  flixer-web     │  ← polls / WebSocket → task status, queue stats,
│  Admin Dashboard│     concurrency controls, cancel/pause, etc.
└─────────────────┘
```

### Responsibilities

**flixer-indexer**
- Discover libraries from flixer.
- Walk library folders according to type (movies / series / music).
- Identify media via filename, NFO, embedded tags.
- Fetch remote metadata + images from all supported providers.
- Probe technical metadata (ffprobe).
- Call flixer’s internal API to create/update media items.
- Expose task status, queue stats, and control endpoints for the admin UI.
- Honour live concurrency settings.

**flixer**
- Own the database schema and all writes.
- Expose internal API for media upsert + library/settings reads.
- Proxy or re-expose task status to flixer-web.
- Store provider API keys and per-library image-storage preference.

**flixer-web**
- Admin dashboard showing live tasks, queue depths, concurrency controls, cancel/retry, etc.

---

## 3. Asynq Queues & Task Model

### Queues (priority + weighted)

| Queue        | Purpose                                      | Default concurrency | Notes |
|--------------|----------------------------------------------|---------------------|-------|
| `scan`       | Walk library folders, discover files         | 1–4                 | Produces downstream tasks |
| `identify`   | Parse filename / NFO / tags → provider IDs   | 8–16                | |
| `metadata`   | Fetch title, overview, cast, ratings, etc.   | 8–16                | Configurable live |
| `images`     | Download + process posters, backdrops, logos | 4–12                | Dedicated workers |
| `probe`      | ffprobe technical metadata                   | 4–8                 | |
| `refresh`    | Forced re-scan of existing items             | same as metadata    | |

Each individual media item (movie file, episode, album, track) is **one Asynq task**.  
Image download and any resizing/thumbnail generation **must** run on the `images` queue.

### Task Types (suggested)

```go
const (
    TypeScanLibrary   = "scan:library"
    TypeIdentifyItem  = "identify:item"
    TypeFetchMetadata = "metadata:fetch"
    TypeFetchImages   = "images:fetch"
    TypeProbeItem     = "probe:item"
    TypeRefreshItem   = "refresh:item"
)
```

Payloads should be small JSON structs containing at least:
- `libraryId`
- `path` (absolute or relative to library root)
- `itemType` (`movie`, `episode`, `series`, `album`, `track`, …)
- any already-known provider IDs
- optional parent IDs (series/season/album)

Tasks should support:
- Retries with exponential backoff
- Timeouts / deadlines
- Unique option (deduplication by path or content hash where sensible)
- Cancellation

### Concurrency Control

- Max concurrency per queue is stored in flixer’s settings table.
- Indexer reads the values at startup and reloads them periodically (or via a control endpoint).
- Admin can change them live from flixer-web; changes take effect without restart.

---

## 4. Data Flow (Happy Path)

1. Admin (or scheduled job) triggers “Scan Library X” in flixer-web.
2. flixer looks up the library row and calls  
   `POST /internal/indexer/scan` with `{ "libraryId": "..." }`.
3. Indexer enqueues a `scan:library` task.
4. Scanner walks the path, respects library type, finds media files + local NFO/images.
5. For every discovered item it enqueues the pipeline:  
   `identify` → `metadata` → `images` → `probe`  
   (or a single orchestrator task that enqueues the steps).
6. After metadata + images are ready, indexer calls flixer’s internal API:

```http
POST /internal/media/items
Content-Type: application/json

{
  "libraryId": "uuid",
  "type": "movie",
  "path": "/media/movies/Inception (2010)/Inception (2010).mkv",
  "providerIds": {
    "tmdb": "27205",
    "imdb": "tt1375666",
    "tvdb": null
  },
  "metadata": {
    "title": "Inception",
    "originalTitle": "Inception",
    "overview": "...",
    "year": 2010,
    "runtime": 148,
    "genres": ["Action", "Science Fiction", "Adventure"],
    "studios": ["Warner Bros. Pictures"],
    "cast": [ ... ],
    "crew": [ ... ],
    "ratings": { "tmdb": 8.4, "imdb": 8.8 },
    "contentRating": "PG-13",
    "tagline": "...",
    "premiereDate": "2010-07-16"
  },
  "images": [
    {
      "type": "poster",
      "path": "/media/movies/Inception (2010)/poster.jpg",
      "provider": "tmdb",
      "width": 2000,
      "height": 3000
    },
    {
      "type": "backdrop",
      "path": "...",
      "provider": "tmdb"
    }
  ],
  "technical": {
    "duration": 8880,
    "videoCodec": "hevc",
    "audioCodec": "truehd",
    "width": 3840,
    "height": 2160,
    "hdr": "dolbyvision",
    "audioChannels": 8,
    "bitDepth": 10
  },
  "nfoPresent": true,
  "localImagesUsed": true
}
```

7. flixer writes to its own tables and returns the new/updated item ID.
8. Task status is kept in Redis (Asynq) and optionally mirrored for the admin UI.

Local NFO + local image files always take priority. Remote providers only fill missing fields.

---

## 5. Metadata Providers (Jellyfin + Plex parity)

### Movies / TV / People / Collections
- **TMDB** (primary)
- **TVDB**
- **OMDb**
- **Fanart.tv**
- **AniDB / AniList** (anime)
- Local NFO

### Music
- **MusicBrainz**
- **TheAudioDB**
- Discogs (optional / lower priority)
- Local tags + embedded art

### Rules
- All provider API keys are stored in flixer’s database (`settings` or a dedicated `provider_credentials` table).
- Indexer reads keys at startup and on refresh.
- Admin can edit keys in flixer-web.
- Providers can be enabled/disabled per library or globally.
- Rate-limit handling and polite backoff are required.
- Local NFO and local images always override remote data for the fields they contain.

---

## 6. Image Storage Policy (per library)

Stored on the library row:

```ts
imageStorage: "alongside" | "cache"
```

- `"alongside"` → write posters, backdrops, logos, etc. next to the media file (or in a conventional sub-folder such as `.media` / `metadata`).
- `"cache"` → everything under  
  `flixer-data/cache/metadata/{libraryId}/{itemId}/…`

The indexer must respect the setting when downloading/saving images.  
The choice is stored in the database and can be changed by an admin.

---

## 7. Internal API Contracts

All endpoints are **internal only** (shared secret, mTLS, or Docker network).  
Never expose them publicly.

### Indexer → flixer

| Method | Path | Purpose |
|--------|------|---------|
| `GET`  | `/internal/libraries` | List libraries (id, name, path, type, imageStorage, enabled providers) |
| `GET`  | `/internal/settings/providers` | API keys + enabled providers |
| `GET`  | `/internal/settings/concurrency` | Current max concurrency per queue |
| `POST` | `/internal/media/items` | Create or upsert a media item + metadata + images |
| `PATCH`| `/internal/media/items/{id}` | Partial update |
| `POST` | `/internal/media/people` | Upsert people (actors, directors, …) |
| `POST` | `/internal/media/genres` | Upsert genres (optional) |

### flixer → indexer

| Method | Path | Purpose |
|--------|------|---------|
| `POST` | `/internal/scan` | Start library scan `{ libraryId, full?: boolean }` |
| `POST` | `/internal/refresh/{itemId}` | Force refresh one item |
| `GET`  | `/internal/tasks` | List tasks (filter by status, queue, libraryId) |
| `GET`  | `/internal/tasks/{id}` | Task detail + progress |
| `POST` | `/internal/tasks/{id}/cancel` | Cancel a task |
| `POST` | `/internal/concurrency` | Live update of worker limits |
| `GET`  | `/internal/stats` | Queue depths, active/idle workers, error rates, etc. |
| `GET`  | `/health` | Health check |

Authentication: shared secret header (e.g. `X-Internal-Token`) or mTLS — same pattern used by flixer-streamer.

---

## 8. Admin Dashboard Requirements (flixer-web)

The admin UI must expose **everything** Jellyfin and Plex show for library scanning, plus:

- Live task table  
  - type, item title/path, progress %, status, started time, ETA, retries, last error
- Queue depth + active / idle workers per queue
- Ability to pause / resume / cancel individual tasks or whole queues
- Live concurrency sliders (write back to flixer settings → indexer)
- Per-library scan progress bars
- Recent failures with one-click “Retry”
- Provider rate-limit / error counters
- Optional real-time updates via WebSocket / SSE (flixer already has Socket.IO)

flixer can either proxy the indexer’s `/internal/tasks` + `/internal/stats` endpoints or maintain a thin cache.

---

## 9. Recommended Go Package Layout

```
flixer-indexer/
├── cmd/
│   ├── server/                 # HTTP API + Asynq server entrypoint
│   └── worker/                 # optional pure-worker binary
├── internal/
│   ├── api/                    # internal HTTP handlers
│   ├── asynq/                  # task types, client, server config, middleware
│   ├── scanner/                # folder walker + library-type detection
│   ├── identify/               # filename parsers, NFO reader, embedded tags
│   ├── providers/
│   │   ├── tmdb/
│   │   ├── tvdb/
│   │   ├── omdb/
│   │   ├── fanart/
│   │   ├── anidb/
│   │   ├── musicbrainz/
│   │   └── audiodb/
│   ├── images/                 # download, save (alongside vs cache), basic processing
│   ├── probe/                  # ffprobe wrapper
│   ├── flixerclient/           # typed HTTP client for flixer’s internal API
│   ├── config/                 # env + dynamic settings reload
│   └── models/                 # shared structs (Library, MediaItem, TaskPayload, …)
├── pkg/                        # any truly public helpers (optional)
├── Dockerfile
├── docker-compose.override.example.yml
├── go.mod
└── README.md
```

---

## 10. Green-field Schema Notes for flixer

The indexer does **not** create these tables. flixer must expose an internal write API that accepts the payloads described above. Suggested minimal entities:

- `libraries`  
  `id`, `name`, `path`, `type` (`movie` | `series` | `music`), `image_storage` (`alongside` | `cache`), `enabled_providers` (jsonb), timestamps
- `media_items`  
  `id`, `library_id`, `type`, `path`, `provider_ids` (jsonb), core metadata columns or a jsonb `metadata` blob, `technical` (jsonb), timestamps
- `images`  
  `id`, `media_item_id`, `type` (`poster` | `backdrop` | `logo` | `banner` | `thumb` | `disc` | …), `path`, `provider`, dimensions, …
- `people` + join table `media_people`
- `genres` + join table (or array/jsonb)
- Optional: `scan_jobs` / `task_history` for long-term history beyond Redis

Exact column design is left to the flixer team; the indexer only cares about the HTTP contract.

---

## 11. Configuration & Deployment

### Environment variables (indexer)

```bash
FLIXER_INTERNAL_URL=http://flixer:8083          # or internal service name
FLIXER_INTERNAL_TOKEN=...                       # shared secret
REDIS_ADDR=redis:6379
REDIS_PASSWORD=
REDIS_DB=0
MEDIA_PATH=/media                               # same mount as flixer/streamer
CACHE_PATH=/cache/metadata                      # when image_storage=cache
CONCURRENCY_SCAN=2
CONCURRENCY_IDENTIFY=12
CONCURRENCY_METADATA=12
CONCURRENCY_IMAGES=8
CONCURRENCY_PROBE=6
LOG_LEVEL=info
```

Concurrency values are defaults; live values come from flixer settings.

### Docker

- Add `flixer-indexer` service to the existing Compose files.
- Share the same `MEDIA_PATH` volume (read-only) and Redis.
- Can scale the worker replicas independently of the API container.
- Healthcheck on `/health`.

---

## 12. Implementation Priorities (suggested order)

1. **Skeleton**  
   - Go module, Asynq client + server, basic HTTP internal API, health endpoint, config loading.

2. **flixerclient**  
   - Typed client for the internal endpoints that flixer will expose (`/internal/libraries`, `/internal/media/items`, …).  
   - Mock or stub the flixer side until the real API exists.

3. **Scanner + Identify**  
   - Library walker that respects type.  
   - Filename / NFO / embedded-tag parsers.  
   - Enqueue downstream tasks.

4. **Providers (start with TMDB + MusicBrainz)**  
   - Fetch metadata.  
   - Respect local NFO priority.

5. **Images queue**  
   - Download, honour per-library storage policy, basic validation.

6. **Probe**  
   - ffprobe integration.

7. **Full pipeline + status endpoints**  
   - End-to-end scan → persist via flixer API.  
   - Task listing, cancel, stats, live concurrency reload.

8. **Admin surface**  
   - flixer proxies the status endpoints.  
   - flixer-web admin page.

9. **Remaining providers + polish**  
   - TVDB, OMDb, Fanart, AniDB/AniList, AudioDB, rate-limit handling, retries, metrics.

---

## 13. Non-Goals (for the first version)

- Writing directly to PostgreSQL.
- Public HTTP API.
- Transcoding or streaming (that is flixer-streamer).
- User-facing search or library browsing (that is flixer + flixer-web).
- Real-time filesystem watching (can be added later; first version is on-demand + scheduled scans).

---

## 14. Success Criteria

- Can scan a library of tens of thousands of items without running out of memory or blocking the main API.
- Individual item failures do not stop the rest of the scan.
- Image work never starves metadata work.
- Admin can see live progress, cancel tasks, and change concurrency without restarting.
- Local NFO + local images always win.
- All persistence goes through flixer’s internal API.
- Provider keys never live in the indexer process longer than necessary and are never logged.

---

## 15. Reference Links

- Flixer backend: https://github.com/flixermedia/flixer
- Flixer web: https://github.com/flixermedia/flixer-web
- Flixer streamer: https://github.com/flixermedia/flixer-streamer
- Asynq: https://github.com/hibiken/asynq
- Jellyfin metadata docs (behaviour reference): https://jellyfin.org/docs/general/server/metadata/

---

**End of specification.**  
An implementer should treat this document as the source of truth. Any deviation (especially around database ownership or queue separation) must be discussed first.
