# Companion API v1

`GET /api/v1/capabilities` accepts normal Navidrome salted-token headers or administrative Bearer authentication. Response: `{"jam":true,"jam_streaming":true,"downloads":false,"fingerprinting":false,"lastfm_metadata":false}`. Disabled downloads return `503 downloads_disabled` on creation/Pre-Save requests. Health omits disabled slskd checks and reports `downloads_enabled`, `state_writable`, `library_writable`, `downloads_writable`.

## Optional Last.fm enrichment

`GET /api/v1/metadata/artist?artist=NAME` and `GET /api/v1/metadata/album?artist=NAME&album=TITLE` require the same normal Navidrome salted-token headers or administrative Bearer authentication. With no `LASTFM_API_KEY`, they return `200 {"enabled":false}` without contacting Last.fm. Enabled lookups return `200 {"description":"Plain text","tags":["style"],"source_url":"https://www.last.fm/music/..."}`. Names must match exactly ignoring case/outer whitespace; no autocorrection, fuzzy artist merges or library writes. Unmatched results return `204`; invalid queries return `400`; unavailable providers return `502` without exposing their response or key. Names are limited to 512 bytes, descriptions to 4000 characters and tags to eight. Matching results are cached for 24 hours with 256-entry eviction. The app fills only missing artist biographies and shows supplementary tags/album descriptions with Last.fm attribution. Existing library artwork, credits and tags are preserved. Configure the supplied proxy's `/api/v1/metadata/` routes with this update.

## Download metadata review

Jobs can enter `needs_review`, with `ReviewSummary` and `CanApproveMetadata`. Retry with `{"retry_id":"id"}` after correcting configuration. When `CanApproveMetadata:true`, explicit edition confirmation uses `{"retry_id":"id","approve_metadata":true}`. Approval is accepted only for a reviewable job, is bound to the reviewed file digests and never bypasses recording/tracklist validation. Changed files require another review. Uncertain matches retain originals and are not silently sent to another provider. Repairs touch newly copied staging files only and produce `.glaze-tags.json` size/digest receipts for crash recovery.

## Member-scoped Jam library

`GET /api/v1/jam/sessions/{sessionId}/library/rest/{endpoint}.view` uses Subsonic parameters `u=<member ID>`, `s=<16–128 hex salt>`, `t=MD5(member_token + salt)`. Never put raw member tokens or host logins in URLs. Allowed endpoints: `ping`, `search3`, `getSong`, `getCoverArt`, `stream`, `getLyricsBySongId`. Search accepts one-character queries and returns at most 50 host-library songs. Audio/lyrics require queued/current tracks; metadata additionally permits returned search hits. Private paths/ratings are removed. Covers must be images. No arbitrary URLs or mutation endpoints are forwarded. Streams preserve Range/If-Range headers and revoke on leave/expiry/end; at most three simultaneous streams/member.

`POST /api/v1/jam/sessions/{sessionId}/tracks` accepts `X-Jam-Member-Token` and multipart parts:

- `metadata`: JSON title/artist/album/duration/suffix, optional samplingRate/bitRate/isExplicit/track/discNumber. No source URLs or credentials.
- `audio`: original audio, at most 100 MiB, with a recognized FLAC/Ogg/WAV/MP4/MP3 header.
- `cover`: optional JPEG/PNG/WebP, at most 512 KiB.

Response: `201 {"track_id":"jam-<opaque ID>"}`. Add this ID with `queue.add`. Phones upload their own authenticated stream; Companion never requests a guest-supplied server URL. Limits: one upload/member, 32 retained contributions/session, 2 GiB global audio storage. Private temporary audio is deleted on contributor leave/session expiry/end/shutdown and never imported into Navidrome. Audio, metadata and art are served through the member library; uploaded tracks return an empty lyrics list. Limits return `429 jam_upload_limit` / `jam_stream_limit`; invalid audio/metadata returns `400`; excessive audio returns `413`.

Deploy the updated Nginx configuration alongside Companion. It permits bounded uploads and disables query-string access logging on streaming routes; the old 8 KiB limit prevents contributions.

Administrative REST routes require `Authorization: Bearer <COMPANION_API_TOKEN>`. Jam creation also accepts an existing Navidrome account via `X-Jam-Navidrome-User`, `X-Jam-Navidrome-Salt`, and `X-Jam-Navidrome-Token` (Subsonic salted-token authentication). Other Jam routes use a private member token. Missing/invalid credentials return `401` or `403`. Method mismatches return `405`; unknown paths return `404`.

## Reserved routes

The following routes remain **stubs**: HTTP `501`, JSON `{"error":"not_implemented","message":"Reserved for a later phase","route":"METHOD /pattern"}`. No body is processed and no operation is performed.

| Method | Path | Reserved purpose |
| --- | --- | --- |
| POST | `/api/v1/library/scan` | Request a Navidrome scan |

## Retired Smart tagging routes

Authenticated owner/admin requests under `/api/v1/tagging/` return `410` with `{"error":"smart_tagging_removed"}`. Scanning, review, approval and automatic tag rewriting are removed. Existing reports and backups remain on disk; no old jobs resume.

## Upcoming albums and Pre-Save

These routes accept any valid account on the configured Navidrome server via salted-token headers, or the loopback administrative Bearer token. Navidrome `getUser` verifies credentials and resolves the canonical account name. Favorites and `saved` flags belong to the authenticated caller; Bearer calls use the configured owner. Credentials are never persisted. Legacy Pre-Saves migrate to the owner, and one account cannot cancel another account’s save. The catalogue cache and acquisition jobs remain shared, so multiple Pre-Saves acquire an album once. Update the proxy from `ops/nginx-jam.conf` for cancellation and explicit release retries.

| Method | Path | Behavior |
| --- | --- | --- |
| GET | `/api/v1/releases` | Upcoming albums from native favorite artists plus explicit Pre-Saves; `followed` distinguishes them |
| GET | `/api/v1/releases?artist_id=<Navidrome ID>` | Upcoming albums for one library artist, including artists not yet favorited |
| GET | `/api/v1/releases/{Apple collection ID}` | Details for a discovered release, or verify an explicitly selected upcoming Apple release |
| PUT | `/api/v1/releases/{id}/presave` | `{"saved":true}` to save; `false` to undo before release/acquisition starts |
| POST | `/api/v1/releases/{id}/retry` | Retry the caller's failed or cancelled saved release after its release date |

Album fields: `id`, `artistId` (Navidrome), `title`, `artist`, `releaseDate` (ISO date), `releaseAt` (UTC epoch milliseconds), `cover`, `trackCount`, `tracks`, `saved`, `followed`, `jobId`, `status`, `error`. Track fields: `id`, `title`, `artist`, `duration` (seconds), `explicit`, `released`. Catalogue IDs are never playable Navidrome IDs. The list is sorted by release time and excludes unfavorited, unsaved albums. Explicit saves remain visible after release so clients can observe acquisition completion. `PUT` returns the album, `409 release_already_started` after release or acquisition submission, `404 release_not_found` for unknown IDs, and `503` for unavailable storage. The service does not accept an independent follow list.

When available, `checkedAt` and `nextCheckAt` report Unix milliseconds for the last check and next automatic retry. Active or completed downloads have no next retry time.

## Automatic acquisition

`POST /api/v1/acquisition/jobs` accepts `{"artist":"Migos","title":"Culture II","kind":"album"}` or `kind:"track"` (the default), and returns `202` with a job ID. `GET /api/v1/acquisition/jobs/{jobId}` returns the current `searching`, `downloading`, `moved`, `rescanned`, or `failed` state; `GET /api/v1/acquisition/jobs` lists jobs newest first. These endpoints accept the admin Bearer token on loopback, or `X-Jam-Navidrome-User` / `X-Jam-Navidrome-Salt` / `X-Jam-Navidrome-Token` for any valid account on the configured Navidrome server. Storage information is available to the same authenticated accounts; download history and imported files are shared. The Nginx site publishes only these job methods; it never accepts the admin token from the app. A duplicate submission returns the existing job. Distinct requests download concurrently, including after a restart; an uncertain slskd submission only blocks resubmitting that same request. Imports each trigger a serialized Navidrome scan after their files arrive. A failed job reports a short error code. Source selection is automatic, using the optional configured provider order.

`GET /api/v1/acquisition/jobs/preview/{albumId}` returns a complete Deezer album response, with all tracklist pages combined. Album IDs are numeric Deezer IDs, never Navidrome IDs. The bounded in-memory cache keeps 128 releases for up to 24 hours. Preview and acquisition share this response. Album submissions may include `catalogue_album_id`; the companion validates the selected release's artist/title and records its verified track count. Existing jobs/manual requests resolve an exact Deezer artist/title/edition first, with MusicBrainz as a fallback when no unique catalogue match exists. Individual track submissions may include `album` to preserve their selected release context.

To retry a failed job, POST `{"retry_id":"<job ID>"}` to `/api/v1/acquisition/jobs`. The server uses the original request metadata, resumes a known transfer after connection/import errors, or creates a new job after a definitive failure. Active jobs and uncertain slskd submissions return `409` and are not duplicated. Retry requires the same authenticated access as creating downloads.

The companion searches slskd, rejects known compilation folders and incompatible editions before ranking audio by title/artist, size/duration, free upload slot and `ACQUISITION_FORMAT_ORDER`, and rejects album folders with a different audio-file count from the verified catalogue tracklist. It submits one batch to an isolated `glaze/<job-id>` directory, waits for every transfer to succeed, verifies file sizes and reads embedded tags to reject a conflicting song/artist/album or compilation copy with optional fingerprint-assisted staged tag recovery, copies into a new artist/title directory without overwriting existing content (path punctuation is escaped), and starts/waits for Navidrome's rescan. Job state is stored under `/downloads/glaze/jobs` so in-progress batches can resume after a restart. Shutdown preserves the resumable state. A failed scan remains `moved` and retries every 30 seconds; this includes jobs whose original provider is now disabled. A `.glaze-job` marker and selected filenames/sizes identify completed copies for crash recovery. Ambiguous slskd enqueues retain `slskd_enqueue_failed` until their original batch can be reconciled; another submission is blocked to avoid duplicate downloads. Optional Qobuz subscription credentials enable streamrip to search Qobuz noninteractively, download a matching FLAC release into the same isolated job area, and apply the same no-overwrite move/rescan checks. `ACQUISITION_PROVIDER_ORDER` determines which source is tried first. Qobuz has been packaging-tested but not live-account-tested.

`GET /live` is unauthenticated process liveness. `GET /` is authenticated service identification.

## Jam sessions (implemented)

Jam creation accepts the companion Bearer token or the creator's Navidrome salted-token headers. It returns a random `invite_token` to the host, which the host may share with guests along with the session ID. Guests submit that invite in the **join JSON body**, never as a URL or header. Create/join return a private, random `member_token`; use it in the **`X-Jam-Member-Token` header** for reads, leave, delete and WebSocket upgrades. No guest needs the companion admin token. Treat invite and member tokens as secrets. Guests do not need an account on the host library; member-scoped streaming is described below.

| Method | Path | Body / response |
| --- | --- | --- |
| POST | `/api/v1/jam/sessions` | `{"name":"Andy"}` → 201 `{session, member_id, member_token, invite_token}`; creator is host |
| POST | `/api/v1/jam/sessions/{sessionId}/join` | `{"name":"Guest","invite_token":"..."}` → 201 `{session, member_id, member_token}` |
| GET | `/api/v1/jam/sessions/{sessionId}` | 200 session snapshot; member token required |
| POST | `/api/v1/jam/sessions/{sessionId}/leave` | No body → 200 `{"status":"left"}`; host leaving ends the session |
| DELETE | `/api/v1/jam/sessions/{sessionId}` | No body → 200 `{"status":"left"}`; host only |

Name: trimmed, 1–80 UTF-8 bytes, no control characters. Create/join bodies are limited to 4 KiB and reject unknown fields/trailing JSON. Invalid input returns 400; missing session 404; invalid membership or forbidden action 403; capacity 429. All REST errors have `{error: code}`. Other participants never receive private member tokens.

Snapshots contain `id`, `host_id`, monotonic `revision`, UTC `expires_at`, `server_time_ms`, `guest_playback`, `members: [{id,name,avatar?}]`, `queue: [{id,track_id,added_by,votes}]`, and `playback: {track_id,playing,position_ms,updated_at_ms,shuffle,repeat}`. Votes are a map of member IDs to `true`; their count is the number of keys. Queue IDs identify individual entries, so the same track may be queued more than once.

## Jam WebSocket (implemented)

Connect to `/api/v1/jam/ws?session_id=<id>` with the member-token header. First event is `{"type":"connected","member_id":"...","session":{...}}`. Every accepted state-changing command broadcasts `{"type":"state","session":{...}}` to **all** connected members, including the sender. Snapshots are serialized atomically; clients should replace their state and ignore older revisions. Reconnect with the same token to receive the current snapshot. A disconnect does not remove membership or transfer host ownership; explicit leave does.

Commands (JSON text):

| Type | Additional fields | Access / effect |
| --- | --- | --- |
| `heartbeat` | `nonce` (string, optional) | Echoes nonce with `server_time_ms`; does not change session state |
| `playback.request` | `action`: `play`, `pause`, `next`, `previous`, `seek`, `shuffle`, `repeat`, `track`, or `queue_item`; `position_ms`, `shuffle`, `repeat`, `track_id`, or `item_id` as required | Any member when guest controls are enabled; forwarded once to the connected host, without changing playback/revision |
| `playback.set` | `track_id`, `playing`, `position_ms`; optional `shuffle`, `repeat` (0 off, 1 one, 2 all), `item_id`, skipped `item_ids`, and remaining `queue_ids` | Host only; confirm actual playback after preparation and atomically consume played/skipped entries and confirm the remaining order |
| `playback.next` | None | Legacy host command consumes first entry; guest calls are forwarded as a `next` request |
| `queue.add` | `track_id`, optional `insertion:"before"` / `"after"` (or legacy `next:true`) | Any member; before/after existing manual additions, ahead of the playlist. With neither field, append. Queue entries expose `queued:true` for manual additions |
| `queue.seed` | `track_ids` (array, up to 1000) | Host only; copy the existing upcoming queue into an empty Jam once |
| `queue.order` | `item_ids` (exact permutation of upcoming entry IDs) | Host only; publish native shuffle order, retaining votes and ownership |
| `queue.restore` | `track_ids` (up to 1000) | Host only; restore upcoming history after previous/repeat-all, retaining matching queue entries and votes |
| `queue.clear` | None | Host only; clear upcoming songs without stopping the current track |
| `queue.vote` | `item_id`, `vote` (boolean) | Any member; set/remove their one upvote; repeated identical vote is a no-op |
| `queue.remove` | `item_id` | Host or entry's creator; remove entry |
| `queue.move` | `item_id`, `to_index` (zero-based) | Any member; reorder upcoming songs |
| `guest_controls.set` | `guest_playback` (boolean) | Host only; allow or disallow guest playback controls, for both nearby and remote listening |
| `profile.set` | `avatar` (base64 JPEG or empty) | Set your own profile image; maximum 12 KiB decoded, 256×256 pixels |

Queue sorting: most votes first, ties in original insertion order. Voting never changes the current song. Leaving removes that member's votes and temporary contributed audio/queue entries; host-library queue entries remain. The host uses native playback for transitions and confirms the resulting song with `playback.set`; the server has no audio player or automatic advancement timer.
Guest playback controls start enabled and can be changed by the host. A forwarded event is `{"type":"playback.request","member_id":"requester","command":{...}}`, delivered only to one host socket. A missing host returns `host_unavailable`; requests are never replayed after reconnecting.

Track IDs are Navidrome IDs, not media URLs: nonempty, at most 256 bytes, no whitespace/control characters. The member library serves permitted host-library/contributed tracks. Contributed IDs have an opaque jam- prefix independent of source-server IDs. Position is 0–604800000 ms. Empty `track_id` is allowed only in `playback.set` with `playing:false, position_ms:0` to clear playback.

All state-changing commands optionally accept `expected_revision`; mismatch returns `stale_revision` without modifying state. Errors go only to the sender as `{"type":"error","error":"..."}`. Codes: `host_only`, `not_item_owner`, `invalid_track_id`, `invalid_playback`, `invalid_vote`, `item_not_found`, `queue_limit`, `stale_revision`, `unknown_command`, `host_unavailable`, `invalid_queue_order`. Repeated votes do not broadcast; use a heartbeat/GET if a caller needs confirmation. Non-idempotent add/next commands must not be blindly retried; reconnect/read first or use `expected_revision`.

Playback synchronization uses a server-clock anchor, not timestamps supplied by a phone. Estimate clock offset with heartbeat round trips (server time minus local send/receive midpoint). While playing, target position is `position_ms + max(0, estimated_server_now_ms - updated_at_ms)`; while paused, exactly `position_ms`. Guests choose their output in a separate popup after joining. Host-phone output stays silent locally; this-phone output fetches audio from the member-scoped Companion library and re-anchors after buffering and state/heartbeat updates. Both modes display the host-confirmed track and position and send the same controls. Only the host advances the shared queue; guests never predict the next track. Networked playback is drift-corrected, not sample-accurate.

End event: `{"type":"session_ended","reason":"host_left"}` or reason `expired`, followed by normal socket closure. Sessions are in memory, expire after 24 hours (checked on subsequent Jam requests/messages and by a one-minute cleanup timer), and are lost on service restart. Shutdown cancels sockets. Limits: 64 sessions, 32 members/session, 1000 queued entries, 4 sockets/member; 512 KiB inbound frames, 20 messages/sec with a 40-message burst. Outbound snapshots may be larger: clients should allow 512 KiB. Slow clients with 16 pending events are disconnected and should reconnect for current state. Invalid JSON/binary frames close the socket. Heartbeat at least every 30 seconds; 75-second idle timeout. Browser origins must match the host; native clients may omit Origin. The container stays loopback-bound; the optional Nginx site exposes Jam and Navidrome-authenticated acquisition routes over HTTPS.

## Health

`GET /health` requires Bearer authentication and performs fresh, parallel upstream checks on every call. HTTP `200` / `status: ok` means Navidrome authenticated, the Companion state folder is writable and, when downloads are enabled, enabled providers authenticated and media folders are readable/writable. Otherwise it returns HTTP `503` / `status: degraded` with the same JSON structure. `/live` stays independent so an upstream outage does not restart a healthy process.

Fields: `service`, `status`, monotonic `uptime_seconds`, UTC `checked_at`, `library_accessible`, `state_writable`, and `dependencies.navidrome` / enabled provider dependencies. Disabled slskd is omitted. Each dependency reports `reachable`, `configured`, `authenticated`, `http_status` (if received), `latency_ms`, and a sanitized `error` (if any). No credentials, upstream URLs, filenames, or response bodies are exposed.

- Navidrome: [OpenSubsonic ping](https://opensubsonic.netlify.app/docs/endpoints/ping/) using a fresh salt/token. A HTTP 200 containing Subsonic `status: failed` is reachable but **not** authenticated.
- slskd: [session check](https://github.com/slskd/slskd/blob/master/src/slskd/Core/API/Controllers/SessionController.cs) using an API key, or web username/password login followed by the session check. This does not contact Soulseek, search, download, or edit slskd configuration.
- Both checks share a five-second deadline; redirects are rejected so credentials cannot follow a redirect to another host. API URLs should use internal Docker names or HTTPS on trusted hosts.

Release dates from the public iTunes catalogue have date precision. `releaseAt` is the UTC calendar-day anchor, not a verified streaming launch instant. Acquisition becomes eligible on that date and retries hourly during release day if unavailable; the phone counts calendar days in its local timezone rather than assuming a launch hour. Preorder preview URLs alone do not mark a track as released.

## Cancellation, explicit release lookup and retry

Acquisition statuses are `searching`, `downloading`, `verifying`, `importing`, `moved`, `rescanned`, `failed`, `needs_review`, `cancelling` and `cancelled`. The old manual acquisition search and DELETE cancellation placeholders are removed; cancellation uses the POST endpoint below.

`POST /api/v1/acquisition/jobs/{jobId}/cancel` uses normal acquisition authentication. Returns `202` with persisted `CancelRequested:true` / `Status:cancelling`, or `200` when already stopping/cancelled, `404` for an unknown job and `409 import_already_started` after the import boundary. This cancels shared server jobs, not library music. Only owned transfers/private staging are cleaned; upstream failure remains `cancelling` across restarts until stopping can be confirmed. Final status is `cancelled`. Cancelled jobs can be explicitly retried through the existing job-creation retry body. Qobuz processes and their children are stopped together on Linux.

`GET /api/v1/releases/{id}` can resolve an unknown numeric **Apple Music** album ID to a verified upcoming release. It never accepts arbitrary URLs or supplied metadata. Explicit selections have no Navidrome `artistId` until one is known; do not treat catalogue IDs as library artist IDs. BitChord+ accepts Apple Music album links locally and sends only the parsed ID. Deezer acquisition catalogue IDs are a different namespace and must never be substituted here.

Release responses include `checkedAt` and `nextCheckAt` as Unix milliseconds when a check/retry is known. `POST /api/v1/releases/{id}/retry` requires the caller to have saved that release and its due job to be failed/cancelled; returns `200` with the new state, `409` for future/active/already-imported jobs and `404` for unavailable saves. Automatic retries run hourly during the first release day, daily afterward. Cancellation suppresses automatic resubmission. A metadata review is not automatically retried.

The updated proxy allows POST on acquisition job subroutes and release subroutes; authentication remains enforced by Companion. Bluetooth nearby discovery exchanges the existing invitation on phones and adds no new public Companion discovery endpoint.
