<div align="center">
  <h1>BitChord+ Companion</h1>
  <p><strong>Your music server's helper: downloads, Pre-Save, and a library that knows what it already has.</strong></p>
  <p>The self-hosted server for <a href="https://github.com/ILostXD/BitChord_Plus">BitChord+ on Android</a>.</p>
  <p><a href="#setup-with-docker-compose">Setup</a> · <a href="#choose-your-download-and-music-folders">Folder mapping</a> · <a href="#configuration-reference">Configuration</a> · <a href="API.md">API</a> · <a href="#troubleshooting">Troubleshooting</a></p>
</div>

Companion connects BitChord+ to **Navidrome**. **slskd** and **Qobuz** are optional download providers. It runs on your server, handles shared listening sessions and downloads, and makes upcoming releases available to the app.

> [!NOTE]
> This is alpha software. The README describes the current source tree; released binaries may lag behind it. Companion is a separate service—it does not install, replace or redeploy your existing Navidrome or slskd containers.


## What it adds to BitChord+

- **Library-aware matching.** Before anything is downloaded, Companion checks Navidrome: the same song on another album, a different edition (deluxe vs. standard), another credit line. It skips what you have, and *asks* when it is unsure (a different cut of a song, an edition you don't own).
- **Lossless first.** slskd results are ranked lossless before lossy, and a second "... flac" search runs before an mp3 is accepted. Qobuz is optional.
- **Dashboard.** Open `https://your-companion-address/dashboard`, sign in with your Navidrome account, and see what needs your decision, what is downloading, what was added, what you already had and what failed with a reason and a retry button. The BitChord+ app shows the same questions as a notification and a dialog.
- **Throttled.** Repeated requests are folded into the job that already exists, failed jobs are not retried by every repeat request, and at most 3 jobs run at once.
- **Pre-Save.** An album you Pre-Save is fetched track by track as its tracks come out, and the missing ones are fetched on release day.

## Features at a glance

| Feature | What Companion does |
| --- | --- |
| **Jam** | Invite links / QR invitations, shared queues, votes, host-controlled playback and guest permissions. Serves host-library audio to invited members; guests can contribute songs from their own libraries. |
| **Album and song downloads** | Searches slskd and optional Qobuz in your chosen order, imports matching files and triggers a Navidrome scan. |
| **Release-aware matching** | Checks tags and optionally verifies audio fingerprints. Repairs only newly downloaded staging copies; uncertain editions require review. |
| **Optional Last.fm metadata** | Adds artist/album descriptions and style tags to BitChord+; fills missing artist biographies. Disabled unless the administrator supplies `LASTFM_API_KEY`. |
| **Upcoming releases** | Uses each user’s native Navidrome artist favorites and public catalogue metadata; no separate Follow list. |
| **Pre-Saves** | Stores explicit saves per account and starts downloads when due, even with the phone closed. |
| **Storage and history** | Reports disk space and shared download history to BitChord+; persists acquisition jobs across restarts. |
| **Multiple users** | Authenticates valid accounts on the configured Navidrome server. Favorites / Pre-Saves are personal; the music library and downloads are shared. |
| **Health checks** | Checks real Navidrome/slskd connectivity and authentication, plus mounted library access. |

There is no web dashboard or library-wide Smart tagging. Downloads follow the optional configured provider order; there is no per-download source picker.

## Setup with Docker Compose

### Recommended: guided setup

BitChord+ can browse/play an existing Navidrome without Companion. Navidrome can run in Docker or through a [native installation](https://www.navidrome.org/docs/installation/).

Clone this repository, then run `python3 ops/setup.py`. The helper asks for Navidrome's address/account and a Docker network. Downloads start **disabled**. If enabled, it asks for slskd's address, **API key or web username/password**, existing music/download directories and the media account's UID/GID. An optional AcoustID application key enables fingerprint recovery.

Passwords are hidden, `.env` is private and written atomically, existing values are preserved, and updating an existing configuration requires choosing that option. The helper offers to start **Companion only**. It does not redeploy Navidrome/slskd or change their folder permissions. It can create a dedicated Docker network for a remote/native Navidrome.

The helper also offers an optional Last.fm metadata key, including when downloads are disabled. Skip it to leave enrichment off; app users do not need a Last.fm account. Lookup uses exact artist/album names with autocorrection disabled, caches matched results for 24 hours (256 entries), and shows Last.fm attribution. It never rewrites files, changes album editions/credits, or replaces artwork. Same-name artists can still be ambiguous, so this is descriptive enrichment rather than proof of identity. It is independent of AcoustID and scrobbling. [Last.fm API](https://www.last.fm/api)

The lightweight service stores catalogue state in its own volume, without music/download mounts:

```sh
# Set DOWNLOADS_ENABLED=false in .env; no slskd/media folders needed.
docker compose -f compose.jam.yaml up -d --build

# Enable downloads with the two media folders configured in .env.
docker compose -f compose.jam.yaml -f compose.downloads.yaml up -d --build

# Check real upstream authentication and enabled folder permissions.
python3 ops/setup.py --check
```

The original `compose.yaml` remains supported for existing full deployments. Omitting `DOWNLOADS_ENABLED` preserves legacy behavior. Pre-Saves require downloads; existing saves remain stored while downloads are disabled. HTTPS/reverse-proxy setup is still needed for access outside your server network. The helper does not obtain certificates or change DNS.

### 1. Check the prerequisites

- A Linux server with Docker Engine and the Compose plugin.
- Working **Navidrome**, reachable from Companion; **slskd** is needed only for Soulseek downloads.
- For downloads only: the music library and completed-download folder accessible on that server, including mounted NAS shares.
- A Navidrome account; for slskd downloads, either an API key or slskd **web** username/password.
- For the phone: an HTTPS reverse proxy with WebSocket support.
- Optional: Qobuz subscription credentials.

The supplied Compose file joins an **existing external Docker network**. Use a network that can reach both services; container names such as `navidrome` and `slskd` resolve only when networking is configured appropriately.

### 2. Get the source and create your configuration

```sh
git clone https://github.com/ILostXD/BitChord_Plus_Companion.git
cd BitChord_Plus_Companion
cp .env.example .env
chmod 600 .env
openssl rand -hex 32
```

Put the generated token into `COMPANION_API_TOKEN` in `.env`. Keep it on the server. Then edit the remaining values:

```dotenv
COMPANION_API_TOKEN=PASTE_YOUR_GENERATED_TOKEN_HERE
COMPANION_BIND_ADDRESS=127.0.0.1
COMPANION_PORT=8088

DOCKER_NETWORK=media
NAVIDROME_API_URL=http://navidrome:4533
NAVIDROME_USERNAME=your_navidrome_account
NAVIDROME_PASSWORD=your_navidrome_password
SLSKD_API_URL=http://slskd:5030
SLSKD_API_KEY=your_slskd_api_key

# These are real paths on the Docker HOST.
NAVIDROME_LIBRARY_HOST_PATH=/srv/music
SLSKD_DOWNLOAD_HOST_PATH=/srv/downloads/soulseek/complete

# Keep these paths with the supplied compose.yaml.
NAVIDROME_LIBRARY_PATH=/music
SLSKD_DOWNLOAD_PATH=/downloads

# Example only: use the actual media account UID and GID.
COMPANION_UID=1000
COMPANION_GID=1000
ACQUISITION_PROVIDER_ORDER=slskd,qobuz
ACQUISITION_FORMAT_ORDER=flac,mp3,other
```

Find the media account’s numeric IDs with `id -u USERNAME` and `id -g USERNAME`; do not assume they are 1000. API URLs are base URLs, without `/rest` or `/api/v0`. They are addresses reachable **inside Companion’s container**, not necessarily the public URLs used on your phone.

If using slskd web credentials, leave `SLSKD_API_KEY` blank and set `SLSKD_USERNAME` / `SLSKD_PASSWORD`. These are **not Soulseek credentials**.

The setup helper also offers optional Qobuz subscription credentials and, when both sources are enabled, a first-source preference. Leave the order unset for `slskd,qobuz`, or use `ACQUISITION_PROVIDER_ORDER=qobuz,slskd`. Unconfigured providers are skipped. Source selection is automatic; BitChord+ has no manual source picker.

### Choose your download and music folders

**Yes—both directories are configurable.** The two `*_HOST_PATH` variables select the folders mounted into Companion.

| Setting | Meaning | Example |
| --- | --- | --- |
| `SLSKD_DOWNLOAD_HOST_PATH` | Host folder corresponding to slskd’s **completed-download directory** | `/srv/downloads/soulseek/complete` |
| `NAVIDROME_LIBRARY_HOST_PATH` | Host music folder scanned by Navidrome | `/srv/music` |
| `SLSKD_DOWNLOAD_PATH` | Same download folder as seen inside Companion | `/downloads` |
| `NAVIDROME_LIBRARY_PATH` | Same music folder as seen inside Companion | `/music` |

With the supplied Compose file, change the **host paths** and keep the container paths shown above. Compose explicitly sets `SLSKD_DOWNLOAD_PATH=/downloads`. If you customize the bind-mount targets, update the corresponding container environment paths too.

#### Example: slskd uses a different container path

Suppose slskd mounts this host folder:

```yaml
# Excerpt from YOUR slskd service, not Companion's compose.yaml
volumes:
  - /srv/soulseek:/app/downloads
```

And slskd’s configuration contains:

```yaml
directories:
  downloads: /app/downloads/complete
  incomplete: /app/downloads/incomplete
```

Then Companion needs:

```dotenv
SLSKD_DOWNLOAD_HOST_PATH=/srv/soulseek/complete
```

**Not** `/app/downloads/complete`—that is a path inside the slskd container. **Not** `/srv/soulseek/incomplete`—Companion imports completed downloads.

The resulting layout is:

```text
Docker host                         slskd container              Companion container
/srv/soulseek/complete       →       /app/downloads/complete  →   /downloads
/srv/music                  →       Navidrome's music mount  →   /music
```

Companion requests a relative slskd destination of `glaze/<job-id>` (the folder keeps this name so existing installations keep working). Therefore that job must appear at:

```text
Host:       /srv/soulseek/complete/glaze/<job-id>/
Companion:  /downloads/glaze/<job-id>/
```

If slskd’s completed files land somewhere else, fix this mapping before trying downloads. Check your slskd configuration’s `directories.downloads` and its Docker volume mapping; [slskd’s configuration reference](https://github.com/slskd/slskd/blob/master/docs/config.md) explains its directory settings. Inspect container mounts with:

```sh
# Replace slskd with your actual container name.
docker inspect slskd --format '{{json .Mounts}}'
```

For a NAS or a different download server, mount the same completed-download storage on the Companion host first. A filesystem path or slskd API URL alone does not provide access to remote files.

#### Permissions

Both host directories must already exist. Companion’s configured UID/GID needs permission to read and remove its completed downloads and create new folders in the library. slskd must also be able to write under the shared completed-download root. Match your existing media account or configure appropriate group permissions / NAS ACLs.

Do not solve this by making the entire library world-writable. Existing deployments changing UID/GID also need ownership or ACLs adjusted for Companion’s own `glaze` state and previously imported folders. Keep private state permissions intact.

### 4. Start and verify

```sh
docker compose up -d --build
docker compose ps
curl http://127.0.0.1:8088/live
python3 ops/check_health.py
```

`/live` only says the process is alive. The authenticated health script reads your local `.env` and verifies the real upstream services without printing credentials. Both Navidrome and slskd should report authenticated access.

Check the mounted paths as the container user:

```sh
docker compose exec companion sh -c 'test -r /music && test -w /music && test -r /downloads && test -w /downloads && echo "Mount permissions OK"'
```

This checks basic directory access, not every nested file’s ACL. Existing Navidrome/slskd services are untouched by this Compose project.

### 5. Connect the phone over HTTPS

Configure your reverse proxy to forward Companion’s app routes to `127.0.0.1:8088`. [ops/nginx-jam.conf](ops/nginx-jam.conf) is an Nginx example: replace its deployment-specific domain and certificate paths with yours. Obtain a valid certificate and enable WebSocket upgrade headers for `/api/v1/jam/ws`.

Keep administrative endpoints private. If the proxy is also a Docker container, its `127.0.0.1` is **not** the host; use an appropriate shared network/upstream address. Do not expose port 8088 publicly merely to work around proxy configuration.

In BitChord+, save the HTTPS address under **Settings → Companion server**. The phone uses the signed-in Navidrome account, not `COMPANION_API_TOKEN`.

## Configuration reference

All values belong in the server’s `.env`; never commit it.

| Variable | Purpose |
| --- | --- |
| `COMPANION_API_TOKEN` | Random administrative token, at least 32 characters. Server only. |
| `COMPANION_BIND_ADDRESS`, `COMPANION_PORT` | Host listener; defaults `127.0.0.1:8088`. |
| `DOCKER_NETWORK` | Existing external network; template default `mediaserver_default`. |
| `NAVIDROME_API_URL` | Navidrome base URL reachable from Companion. |
| `NAVIDROME_USERNAME`, `NAVIDROME_PASSWORD` | Service account used for health, scanning and the configured owner’s background favorite checks. |
| `SLSKD_API_URL` | slskd base URL reachable from Companion. |
| `SLSKD_API_KEY` | Preferred slskd API authentication. |
| `SLSKD_USERNAME`, `SLSKD_PASSWORD` | Alternative slskd web login when no API key is set. |
| `NAVIDROME_LIBRARY_HOST_PATH`, `SLSKD_DOWNLOAD_HOST_PATH` | Host bind-mount sources; see [folder mapping](#choose-your-download-and-music-folders). |
| `NAVIDROME_LIBRARY_PATH`, `SLSKD_DOWNLOAD_PATH` | Container paths; `/music` and `/downloads` in the supplied Compose file. |
| `COMPANION_UID`, `COMPANION_GID` | Media account IDs; default `10001:10001`. |
| `ACQUISITION_FORMAT_ORDER` | All three, once each: `flac,mp3,other`, in preference order. |
| `ACQUISITION_PROVIDER_ORDER` | `slskd,qobuz` or `qobuz,slskd`. |
| `ACQUISITION_STALL_TIMEOUT_SECONDS` | Switch stalled slskd peers after 180 seconds without byte progress by default; accepts 30–1800. Locally queued transfers are exempt. |
| `QOBUZ_USERNAME`, `QOBUZ_PASSWORD` | Optional subscription login. If either is blank, Qobuz is skipped. |

After editing configuration, run **`docker compose up -d`**, not just `docker compose restart`. Rebuild when source or Dockerfile changes.

## How downloads work

1. The app submits an artist, title and song/album request. Catalogue album requests include the selected Deezer release ID; catalogue song requests also carry their album title.
2. Companion searches the configured providers in order. slskd ranking excludes conflicting marked editions and obvious compilations before preferring quality or free upload slots.
3. Album track counts must match the selected catalogue release. Older/manual requests resolve an exact artist/title catalogue match, with MusicBrainz fallback.
4. Files download into an isolated job directory. Size/count checks run, followed by **read-only embedded-tag checks** for the requested song, artist, album and unwanted compilation status.
5. A rejected candidate is not published; the provider chain can try its next source. An accepted candidate is copied into a new artist/release folder, then Navidrome is rescanned.
6. The app refreshes its library and shows **Added to library**.

A Bonus Edition should not silently replace a standard album, and a compilation should not replace an artist's single. When tags fail validation, optional [Chromaprint](https://acoustid.org/chromaprint/) / [AcoustID](https://acoustid.org/webservice) recovery checks the actual recording against title, artist and duration. Album recovery requires the complete requested catalogue tracklist and a unique match for every file. Results are cached for up to 30 days (256 entries); requests respect the service's rate limit.

Set `ACOUSTID_API_KEY` to your registered **application key**; the image includes `fpcalc`. Uncertain/weak/wrong matches stop as **Needs review**, preserving original downloads. Missing configuration can be retried after adding the key. If every recording matches but album identity/edition is uncertain, BitChord+ offers explicit confirmation before importing. A fingerprint identifies a recording, not its mastering/edition or source quality.

Corrected tags are written only to the new staging copy. Audio, artwork and unrelated tags are preserved. Originals are deleted only after successful promotion; a digest receipt makes corrected copies recoverable after a crash. Existing library files are never retagged.

Existing destination folders are never overwritten. Path punctuation is escaped on disk—for example `AC/DC` becomes `AC%2FDC`—without changing metadata. Downloads/history are shared across valid Navidrome users. Identical requests are deduplicated.

Jobs persist across restarts. Failed Navidrome rescans retry every 30 seconds without downloading again. A `.glaze-job` marker identifies the job’s imported folder. Uncertain slskd enqueue responses are reconciled against the original destination; `slskd_enqueue_failed` is not blindly retried because that could duplicate transfers. Qobuz uses pinned streamrip 2.1.0; live behaviour depends on your account and catalogue availability.

Rejected/offline slskd peers and stalled remote queues or transfers automatically try the next matching peer. Any byte progress resets the stall timer. Before switching, Companion cancels and confirms completion of its own previous batch, then removes only that job's known partial files. Failed peers are excluded for the rest of the job; exhausted matching results fall through to the next configured provider. Cancellation/API outages retry after 30 seconds without starting a competing batch. Manual transfers are untouched.

Smart tagging has been removed. Existing reports/backups are retained on disk, and older promotion receipts remain readable for crash recovery. No existing library files are scanned or retagged by this feature.

## Lossless only, found several ways

Companion never adds a lossy file by itself: an slskd result is only considered when every file is lossless (FLAC, WAV, AIFF, ALAC). Each request is searched several ways, stopping at the first lossless match: as asked; a song inside its album (album artist + album, taking just that file); the lead credit with bracketed parts dropped; and the album named in a `(from …)` title. Titles made only of symbols are spelled out (`?` → `Questionmark` / `Question Mark`), and search text keeps letters and digits only.

If nothing lossless exists, the app can add JioSaavn's copy instead (off by default, switch on the Requests page): it hands Companion the song's stream (only `*.saavncdn.com` addresses are accepted), and Companion downloads, tags and files it. Albums come one song per call.

The library check also understands an artist's other names: when your library has an album of the same title under a different credit, MusicBrainz aliases decide (Kanye West / Ye).

## Singles to albums

Two settings on the app's Requests page (`GET`/`POST /api/v1/acquisition/jobs/settings`), both off by default:

- **Replace singles with the album.** When an album is added, its songs your library has on single releases (1–3 songs, named after the song, same artist) are moved to `<library>/.replaced-singles/<date>/` (Navidrome skips hidden folders; nothing is deleted), the library is rescanned, and every playlist that held them is rewritten in its own order with the album's tracks. Only the configured account's playlists can be edited.
- **Get the whole album** once *one*, *half* or *all* of its singles are in your library: after a song arrives, its parent album and that album's singles come from Deezer.

## Requests dashboard

Jobs carry artwork (sent by the app, or looked up on Deezer in the background). Entries can be hidden and restored (`POST /api/v1/acquisition/jobs/hidden`), and `POST /api/v1/acquisition/refresh` re-checks failures against the library and retries the newest ones still missing. Of several attempts at one request, the newest counts.

## Listen Together

Companion also runs BitChord's Listen Together party server (the upstream backend in [`listen/`](listen/NOTICE.md)) under `/api/parties` and `/ws/parties`. BitChord+ uses it automatically when no other party server is set. With nginx in front, run `sudo bash ops/enable-parties.sh` once to let these routes (and the refresh route) through.

## Jam

The host creates a session and shares a link / QR invitation. Guests receive private member tokens and share a queue with voting, reordering and playback requests. The host confirms actual playback and controls whether guests may play, pause, seek, skip, shuffle and repeat.

Guests choose host-phone output or their own phone. Invitations authorize a read-only session library, so guests can hear host-library songs without a Navidrome account or with an account on another server. The host's credentials stay in Companion memory and are never sent to guests.

Signed-in guests keep BitChord+'s normal library, search and song gestures. Add-to-queue contributes their song to the active Jam; tapping Play contributes it and requests playback when the host permits controls. Their phone uploads original audio, artwork and basic metadata to temporary Companion storage; their server URL/login is never shared. After upload, everyone can hear the song in the shared queue. This works even if that library is only reachable from the guest's phone. The upload completes before queueing, so the first contribution can take time. Guests without a library account use **Continue as guest — join a Jam** or open an invite link; that mode is explicitly for joining, listening, searching the host's songs and using the shared queue, rather than hosting or browsing a personal library.

Limits: 100 MiB/song, 32 contributed songs/session, 2 GiB across Companion, one upload/member, three concurrent streams/member. Private temporary audio is removed when its contributor leaves or the session expires/ends, and on shutdown. Host-library queue entries remain after guest departure. Contributed songs currently have no synchronized lyrics; host-library lyrics are proxied read-only.

Deploy the updated `ops/nginx-jam.conf`: Jam routes need 101 MiB upload limits, unbuffered streaming and no query-string access logging. Byte ranges support seeking. The bridge does not expose favorites, downloads or administrative endpoints. Sessions are in memory, expire after 24 hours and end on restart. See [API.md](API.md).

## Upcoming releases and Pre-Saves

Artist favorites are follows: the app’s authenticated feed requests read that user’s native Navidrome artist stars. Apple’s public catalogue supplies upcoming albums and track previews; ambiguous artist names are skipped. A serial worker shares the catalogue cache across users, normally for six hours, with shorter refreshes near release day. The configured service account also gets hourly background favorite checks.

Pre-Saves are stored **per account** in `/downloads/glaze/upcoming-albums.json`. Only explicit saves schedule downloads; favorites alone do not. The server checks due dates every minute. Availability failures retry hourly during release day and every 24 hours afterwards; uncertain slskd enqueues are never automatically resubmitted.

Unfavoriting removes announcements from that user’s feed but retains explicit saves. Several users saving one release still share the normal deduplicated import. Catalogue dates are calendar-day anchors, not guaranteed streaming launch timestamps. Preview URLs alone do not establish that a track is released.

## Updates, backups and logs

```sh
# After choosing/updating to the source version you want:
docker compose up -d --build
docker compose logs --tail=100 companion
python3 ops/check_health.py
```

Back up `.env`, your music library and the `glaze` subdirectory under the completed-download mount. That directory contains acquisition jobs, pending downloads, Pre-Saves and any old tagging reports/backups. Jam sessions are intentionally not persisted.

For a consistent state backup, stop Companion first and restart it after the backup. This ends active Jams. Keep backups containing credentials or private state secure. Do not delete job markers or receipts from imported folders while jobs may need recovery.

## Troubleshooting

| Symptom | Likely check |
| --- | --- |
| External network not found | Set `DOCKER_NETWORK` to the existing network that reaches Navidrome/slskd. |
| `/live` works but the app fails | Verify HTTPS routes / WebSocket proxying and the phone’s Companion URL. |
| Health says authentication failed | Check the correct service credentials. slskd web login is different from Soulseek login. |
| `download_directory_missing_or_unsafe` | Check completed-download host path, slskd destination and bind mounts; symlinked job directories are rejected. |
| Permission denied / copy failed | Check media UID/GID and host or NAS ACLs for both mounts. |
| `download_release_mismatch` | The embedded album differs from the requested release, including edition. The candidate was not imported. |
| `download_is_compilation` | Various Artists / compilation metadata conflicted with an artist request. |
| `download_metadata_unreadable` | TagLib could not inspect the file. Use the supplied image with its helper, and check file integrity. |
| `library_destination_exists` | A folder already occupies the destination. Companion will not overwrite it; inspect it manually. |
| Stuck at `moved` | Files arrived, but Navidrome’s scan is failing or pending. Check its logs and credentials. |
| `slskd_enqueue_failed` | Inspect the original slskd batch/job destination before retrying; its submission outcome is uncertain. |
| No upcoming releases | Verify artist favorites and public catalogue coverage. Not every artist/release is announced. |

## Development and API

Use **Go 1.26+**. Docker builds bundle streamrip and a small **read-only TagLib helper**; a bare Go binary alone cannot inspect/download every supported source.

```sh
go test ./...
docker compose build
```

Run `python3 ops/test_setup.py` to check the helper, including both slskd login modes. With a built Docker image, `python3 ops/test_container.py` checks the Compose configurations and a non-root, read-only Jam-only container against a local fake Navidrome; it needs no real accounts.

Generated-audio checks for the native helper are in [ops/test_taglib.py](ops/test_taglib.py); they require ffmpeg and Mutagen and verify that reads preserve file bytes, staged repairs preserve decoded audio/art/custom tags, and writes without explicit repair mode are rejected.

[API.md](API.md) documents authenticated REST routes, Jam WebSockets, releases and storage. [The Android repository](https://github.com/ILostXD/BitChord_Plus) covers user workflows and builds. Public catalogue browsing needs no extra key; optional fingerprint lookup needs an AcoustID application key.

### New configuration flags

| Setting | Behavior |
| --- | --- |
| `DOWNLOADS_ENABLED` | `false` disables all download providers/mount requirements. Omit to preserve existing behavior. |
| `ACOUSTID_API_KEY` | Optional application key for fingerprint recovery of newly downloaded audio. |
| `COMPANION_STATE_PATH` | Catalogue/Pre-Save state directory. Defaults to existing download state, or temporary storage without downloads. Lightweight Compose provides `/state` in a dedicated volume. |
| `LASTFM_API_KEY` | Optional public Last.fm metadata key. Blank disables all Last.fm requests. Kept on Companion; never sent to phones. |

Upgrade Companion, its reverse proxy, and BitChord+ together for cross-library Jam audio. Older state-only Companion versions cannot serve the new guest player. Jam remains drift-corrected rather than sample-accurate and needs host-server bandwidth for each listening phone.

Nearby discovery is handled by BitChord+ over Bluetooth, with opt-in advertising during playback and foreground discovery. Accepting a nearby invitation makes the host create/reuse the normal Companion Jam automatically; no separate queue, cloud discovery registry or shared Navidrome login is required. Guests still need network access to the host's Companion. In-person audio defaults to the host's output. QR/link invites remain available when Bluetooth discovery is unavailable.

Authenticated server users can cancel a shared acquisition job before library import starts. Stop intent is persisted across restarts; only owned slskd transfers and private job staging are cleaned. Unreachable sources remain `cancelling` until stopping can be confirmed. An importing/already-added release cannot be cancelled or removed this way. Update reverse-proxy rules from [ops/nginx-jam.conf](ops/nginx-jam.conf): job subroutes now permit POST, and release subroutes permit GET/PUT/POST.

Pre-Saves expose download stages, `checkedAt` and `nextCheckAt`, and allow an explicit retry after failure/cancellation. Automatic retries are hourly in the first release day, then daily; a cancelled job stays cancelled until explicitly retried. Explicit selection of a future Apple catalogue ID bypasses ambiguous artist-name discovery, while retaining catalogue verification. These checks improve recovery but cannot guarantee public catalogue coverage or source availability on release day.


## Keeping it up to date

`ops/auto-update.sh` pulls this repository and rebuilds the container when there is something new. Run it from cron, for example every ten minutes:

```sh
*/10 * * * * /path/to/BitChord_Plus_Companion/ops/auto-update.sh >> /path/to/BitChord_Plus_Companion/update.log 2>&1
```

It only rebuilds when `git pull` brings in new commits, and the build runs the test suite first, so a broken commit never replaces the running service.
