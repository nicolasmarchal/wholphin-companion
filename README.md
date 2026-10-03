# Wholphin Companion

License: GPL-2.0-only. This is an independent repository from Wholphin; its
module path is provisional until the final repository URL is assigned.

`companion` is the optional, LAN-side backend for interactive release selection.
It keeps Radarr, Sonarr, Jellyfin and qBittorrent credentials off the Android
device. Seerr is deliberately **not** proxied: Wholphin's existing Seerr flow
remains the feature-flagged fallback.

The API contract is source-first in [`api/openapi.yaml`](api/openapi.yaml). The
runtime uses Go's `net/http`; SQLite is the only runtime dependency outside the
standard library.

## Security invariants

- Only opaque, expiring selection tokens leave this process. Tracker URLs,
  magnets, passkeys, Arr API keys and qBittorrent credentials never do.
- Jellyfin credentials are used once to exchange for a BFF session and are
  neither stored nor logged. BFF session tokens are hash-only. Selection
  tokens are indexed/validated by SHA-256; an AES-256-GCM encrypted copy of
  each token and its exact Arr payload exists only until consumption or expiry
  cleanup so a live search can be rehydrated without exposing tracker data.
  Consumption wipes the token copy; completion of dispatch wipes the payload.
- Every download confirmation requires an `Idempotency-Key`. A candidate can
  create only one job, even when different keys race.
- Movies and series first created by the BFF are added unmonitored and with all
  automatic-search flags disabled. Pre-existing Arr items are left untouched.
  Only the exact cached release selected by its opaque token is posted to Arr.
- A timeout after an Arr grab is treated as `dispatch_uncertain`; the service
  reconciles queue/history and never blindly re-grabs. Correlation requires a
  server-only SHA-256 fingerprint of the selected Arr GUID plus a corroborator
  present in history (indexer name, download URL/info-hash, or indexer ID);
  title equality alone is never accepted. After a crash,
  an in-flight dispatch is restored as uncertain. Such a job cannot be
  cancelled or superseded until its Arr identity is known, because doing so
  could create a duplicate.
- qBittorrent is metrics-only. Radarr/Sonarr remain responsible for grabbing,
  importing and hardlinking/moving downloads.
- Jellyfin availability requires a matching provider ID and a successful
  PlaybackInfo response containing a media source for the requesting Jellyfin
  user. TVDb hints are corroborated against the TMDb identity through Sonarr.
- Secret-bearing HTTP clients refuse redirects. Public status text is an
  allow-listed projection; raw Arr/qBittorrent messages, paths and URLs never
  enter API responses.
- Polling is canonical for the MVP; Arr webhooks only wake reconciliation.
  There is no SSE stream. Unchanged jobs use durable exponential backoff with
  stable jitter, capped at five minutes; a webhook forces an immediate pass.
  Every upstream call remains timeout-bounded. Identical observations do not
  append events. Audit events are retained for 30 days and terminal jobs for
  90 days.

Sonarr can emit an import-history event without `downloadId` (notably some
REPACK flows). The BFF deliberately does not associate such an event by title:
title collisions are possible. The acquisition remains `importing` until Arr
exposes a strongly correlated event. This fail-closed behavior may require
operator review, but it cannot silently claim that another release was the one
selected.

Cancellation is disabled by default. When enabled it is serialized in-process,
refused after import and refused while dispatch identity is uncertain. An
ambiguous upstream DELETE leaves the acquisition active for reconciliation;
the MVP does not expose a durable `cancel_uncertain` workflow.

## Configuration

No secret has a default. Supply secrets through root-owned files or a container
secret mechanism, then expose their contents as environment variables to this
process. Do not commit a populated environment file.

Required:

```text
BFF_LISTEN_ADDR=127.0.0.1:8090
BFF_DATABASE_PATH=/data/companion.db
BFF_DATA_KEY=<base64 of exactly 32 random bytes>
BFF_ALLOWED_JELLYFIN_USER_IDS=<comma-separated Jellyfin user UUIDs>
RADARR_URL=http://radarr:7878
RADARR_API_KEY=<secret>
RADARR_ROOT_FOLDER=/data/media/movies
RADARR_QUALITY_PROFILE_ID=<integer>
SONARR_URL=http://sonarr:8989
SONARR_API_KEY=<secret>
SONARR_ROOT_FOLDER=/data/media/tv
SONARR_QUALITY_PROFILE_ID=<integer>
JELLYFIN_URL=http://jellyfin:8096
JELLYFIN_API_KEY=<dedicated server-side secret>
```

Optional:

```text
BFF_ALLOW_ALL_JELLYFIN_USERS=false
BFF_ALLOW_CANCEL=false
BFF_SESSION_TTL=12h
BFF_SELECTION_TTL=10m
BFF_RECONCILE_INTERVAL=10s
BFF_WEBHOOK_SECRET=<shared Radarr/Sonarr Connect secret>
QBITTORRENT_URL=http://qbittorrent:8080
QBITTORRENT_USERNAME=<secret>
QBITTORRENT_PASSWORD=<secret>
```

The base Compose template mounts only mandatory secrets. Enable webhook and/or
qBittorrent support in a private Compose override that adds the corresponding
`*_FILE` variables and secret mounts; this keeps those files genuinely optional.
Keep `stop_grace_period` greater than `BFF_UPSTREAM_TIMEOUT` if either value is
customized.

If `QBITTORRENT_URL` is absent, progress comes from Arr queue data. If Sonarr
cannot resolve `tmdb:<id>` on the deployed version, clients must also send the
series TVDb ID; Sonarr lookup must still corroborate that it belongs to the
requested TMDb series. The BFF returns `series_mapping_required` or
`subject_identity_mismatch` rather than guessing.

Clients rehydrate with `GET /v1/acquisitions?active=true&tmdbId=...` and should
also pass `mediaType=movie|tv` to keep the independent TMDb movie and TV
namespaces distinct. Results are always scoped to the authenticated BFF user.

Run locally:

```sh
go test ./...
go run ./cmd/companion
```

Production must put the listener behind LAN/VPN HTTPS. Do not expose Arr,
qBittorrent or this service directly to the public Internet.

## Upstream contracts

The adapters are intentionally limited to documented/service-owned APIs:

- [Radarr API v3 source](https://github.com/Radarr/Radarr/tree/develop/src/Radarr.Api.V3)
- [Sonarr API v3 source](https://github.com/Sonarr/Sonarr/tree/develop/src/Sonarr.Api.V3)
- [qBittorrent WebUI API](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-4.1%29)
- [Jellyfin API](https://api.jellyfin.org/)

Pin the deployed service versions and run the integration suite against those
versions before enabling acquisition. Unknown JSON fields are tolerated, but
missing release identity, provider mapping or playback evidence fails closed.

## Container image

The included multi-stage `Dockerfile` builds a CGO-free binary for the target
architecture and runs it as a non-root distroless user. `compose.example.yml`
is a template only: it binds the API to host loopback, expects a pre-existing
private `media-backend` network, mounts SQLite on a named volume, and reads
credentials from untracked files under `secrets/`. Copy `.env.example` to
`.env`, replace identifiers/profile IDs/paths, create the secret files with
restrictive permissions, and terminate HTTPS at a LAN/VPN reverse proxy.
