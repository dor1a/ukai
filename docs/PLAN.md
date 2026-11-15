# ukai plan

> Status: all five phases are built. The image is amd64 only. Values marked `draft` are starting points and may change.

## Summary

ukai is a small self-hosted web app that downloads YouTube videos and audio with yt-dlp, and keeps subscriptions to channels up to date. It replaces ytdl-material in a homelab.

| Item | Detail |
| -- | -- |
| First environment | Homelab, docker compose on one host, behind Cloudflare Access |
| Shape | One Go binary plus the yt-dlp release binary, ffmpeg and Deno in one image |
| Storage | JSON files in a data directory. Downloaded files go to one configured directory |
| UI | HTMX with server rendering |
| Sign-in | Generic OIDC, or none when a proxy in front already handles it. The choice is explicit |
| License | MIT |

## Why

ytdl-material bundles yt-dlp with the Python of its image. When yt-dlp dropped support for the Python version in that image, every download failed until the image was patched by hand. ukai runs yt-dlp's standalone release binary as a child process, so the app never depends on a Python version and yt-dlp can be updated on its own.

## Design principles

1. **yt-dlp is a separate program.** ukai starts it as a child process and reads its output. It is never imported as a library.
2. **No account cookies.** ukai does not pass YouTube account cookies to yt-dlp, so Premium-only formats are out of scope. yt-dlp's documentation warns that using account cookies can get the account blocked.
3. **Sign-in is chosen explicitly.** `UKAI_AUTH` must be `oidc` or `none`. If it is missing, ukai refuses to start, so a missing OIDC setting never leaves the app open by accident.
4. **OIDC stays generic.** Only standard OIDC discovery, authorization code with PKCE, and an optional group claim. No code for a particular provider (Keycloak, Authentik and Cloudflare Access all work through the same settings).
5. **No real song or video URLs in the repository.** Tests use a fake yt-dlp script and made-up IDs. The README shows placeholder URLs.
6. **Keep dependencies small.** Mostly the standard library, plus `go-oidc` and `oauth2` for sign-in. HTMX and CSS are embedded in the binary.
7. **ukai downloads; it does not organise.** Files land in one directory. Renaming and tagging belong to other tools.

## Layout

```
 browser ──▶ (Cloudflare Access or nothing) ──▶ ukai :8080
                                                 │  UKAI_AUTH=oidc → OIDC sign-in
                                                 │  UKAI_AUTH=none → open
                                                 │
            ┌────────────────────────────────────┴────────────────────────┐
            │ web       HTMX pages: submit URL, jobs, subscriptions        │
            │ jobs      queue and workers (UKAI_CONCURRENCY at a time)     │
            │ subs      periodic check of each subscription                │
            │ ytdlp     runs yt-dlp, reads its printed lines and formats   │
            │ store     JSON files, atomic writes (temp file + rename)     │
            └───────┬───────────────────────────┬─────────────────────────┘
                    │                           │
           yt-dlp ─▶ ffmpeg (merge, MP3)    UKAI_DATA_DIR
           (Deno solves YouTube JS)          subscriptions.json, jobs.json,
                    │                        archive/<subscription>.txt
                    ▼
             UKAI_DOWNLOAD_DIR

 Discord bot, scripts ── token API (phase 5) ──▶ ukai
```

## Settings

All settings are environment variables. Defaults marked `draft` may change.

| Variable | Default | Meaning |
| -- | -- | -- |
| `UKAI_AUTH` | none set, required | `oidc` or `none` |
| `UKAI_LISTEN` | `:8080` | HTTP listen address |
| `UKAI_DATA_DIR` | `/data` | subscriptions, job history, download archives |
| `UKAI_DOWNLOAD_DIR` | `/downloads` | where finished files go |
| `UKAI_CONCURRENCY` | `2` (draft) | downloads running at the same time |
| `UKAI_JOB_HISTORY` | `200` (draft) | finished jobs kept in `jobs.json` |
| `UKAI_SUBS_INTERVAL` | `6h` | how often every subscription is checked |
| `UKAI_YTDLP` | `/usr/local/bin/yt-dlp` | yt-dlp binary |
| `UKAI_PUBLIC_URL` | | where browsers reach ukai, required with `oidc` |
| `UKAI_OIDC_ISSUER` | | issuer URL, required with `oidc` |
| `UKAI_OIDC_CLIENT_ID` | | required with `oidc` |
| `UKAI_OIDC_CLIENT_SECRET` | | required with `oidc` |
| `UKAI_OIDC_ALLOWED_GROUP` | `ukai-users` | only members of this group may use ukai |
| `UKAI_OIDC_GROUPS_CLAIM` | `groups` | claim that holds the groups |
| `UKAI_OIDC_SCOPES` | `openid profile email` | scopes to request |
| `UKAI_SESSION_KEY` | | key that signs the session cookie, at least 32 bytes, required with `oidc` |
| `UKAI_GRAVATAR` | `false` | show the signed-in user's Gravatar |
| `UKAI_API_TOKENS` | empty | names of the API's callers; empty turns the API off |
| `UKAI_API_TOKEN_<NAME>` | | each caller's token, at least 32 bytes |

## Phases

### 1. Download one URL

- Submit a URL with a choice of video or audio. Video defaults to the best quality. Audio defaults to the best original stream, kept as it is without re-encoding.
- Other qualities are choices. Before a lookup, video offers Best only and audio offers Best plus MP3 at 320, 256, 192 or 128 kbps. "Show qualities" asks yt-dlp for the formats of that URL (`--dump-single-json`) and adds every height the video has (a cap: yt-dlp takes the largest at or below it) and every original audio format. Video sizes are counted by the shorter side, as yt-dlp's `res` and YouTube do, so a portrait 1080x1920 video is 1080p. At most two lookups run at once, each for at most 90 seconds.
- An original audio format belongs to the URL it was looked up for. The server keeps each lookup in memory for an hour (at most 256 URLs) and accepts an original format only if that lookup of the same URL listed it. A download with that format and a different URL is refused. After a restart or an hour, the user looks the qualities up again.
- File names carry the quality, so two qualities of one video never share a file: `Title [id] 1920x1080.webm`, `Title [id] 251.opus` (format ID), `Title [id] 320k.mp3`.
- A queue with `UKAI_CONCURRENCY` workers. Jobs can be cancelled.
- One yt-dlp run per job. Metadata comes from `--print before_dl:` lines and the final path from `--print after_move:`, progress from `--progress-template` lines. The jobs page shows them with HTMX polling while anything is queued or running.
- `store` behind an interface, backed by JSON files written atomically. Only the last `UKAI_JOB_HISTORY` finished jobs are kept. When a change that cannot be taken back (such as a running job's result) fails to save, memory stays the truth: the jobs page warns, and ukai saves the whole list again every 5 seconds until it works.
- One log line per job event (queued, started, done, failed, canceled, requeued) with the job ID.
- `/healthz` for the container health check. It stays healthy while saves fail, since a restart would queue finished jobs again.
- Tests run against a fake yt-dlp script that prints recorded metadata and progress lines.

### 2. Sign-in

- Port `internal/auth` from noroshi: discovery, authorization code with PKCE, signed session cookie, sign-out (also at the provider when it supports RP-initiated logout).
- `UKAI_OIDC_ALLOWED_GROUP` replaces noroshi's admin group. A group is always required; the default is `ukai-users`.
- The top bar shows the signed-in user's initial and name, and their Gravatar with `UKAI_GRAVATAR=true`, as noroshi's admin pages do.
- An HTMX request without a session gets `HX-Redirect` to the sign-in page, so a poll after the session ended reloads the page instead of fetching the provider's page into the list.
- Startup check: `UKAI_AUTH` missing or unknown, or `oidc` without its required settings, stops ukai with a message that names the missing variable.

### 3. Subscriptions

- Add a channel or artist URL with one of two modes, chosen per subscription:
  - `all` ("Everything, then new"): download everything the channel has, then keep up with new uploads.
  - `new` ("Only new uploads"): download only what appears after the subscription is added.
- Each subscription has its own yt-dlp download archive, so items are never downloaded twice.
- For `new`, the first check lists the channel with `--flat-playlist` and writes every video to the archive in yt-dlp's `<extractor> <id>` format, without downloading.
- Checks run every `UKAI_SUBS_INTERVAL` and can be started by hand. Every subscription is checked each round, unlike ytdl-material, which checks one per round. A later check lists only the newest 50 videos.
- New videos are queued as ordinary jobs, oldest first, into a folder named after the subscription. A video that failed three times is left alone.
- A channel's main URL lists its tabs (Videos, Shorts), so ukai uses its Videos tab.

### 4. Image and deployment

- Multi-stage Dockerfile: build the Go binary, then a runtime image with ffmpeg, Deno and the yt-dlp release binary. The yt-dlp version is pinned and its file is checked against the SHA-256 sums published with the release.
- GitHub Actions: tests on every push, image to `ghcr.io/dor1a/ukai` on tags. Built for amd64 only: ffmpeg is compiled on the target platform and the runtime stage runs apt, so arm64 would need QEMU.
- Compose example for one host, in the README. The port stays on the internal network; outside access goes through the proxy in front.
- A new yt-dlp comes with a new image: the Dockerfile's pinned version and SHA-256 are raised and a new tag is released. ukai does not update yt-dlp inside a running container, so the checksum check always holds.

### 5. API for bots and scripts

- A token-protected API to submit a job (`POST /api/jobs`) and follow it (`GET /api/jobs/{id}`), for a Discord bot or scripts on the same private network.
- Tokens are separate from OIDC and work with either `UKAI_AUTH` value. Each caller has its own: `UKAI_API_TOKENS` lists names, and `UKAI_API_TOKEN_<NAME>` holds each token (at least 32 bytes). The log names the caller. Without names the API does not exist.
- A job takes video at the best quality or a height cap, or audio at the best original stream or an MP3 bitrate. An original audio format needs the lookup in the page, so the API refuses it.

## Out of scope

- Account cookies and Premium-only formats
- Renaming, tagging and moving files into a media library
- Accounts, quotas or per-user history inside ukai
- Promising support for sites other than YouTube. yt-dlp may handle them, but ukai is tested against YouTube only

## Branches and releases

- `dev` is the default branch. `main` is created at the first release and follows releases.
- Releases are tags `vX.Y.Z`. Each tag builds an image.

## Decisions

| Item | Decision |
| -- | -- |
| How to update yt-dlp | Rebuild the image with a new pinned version. Rebuilding keeps the checksum check |
| API scope | Submit and status. Status lets a bot say when a download is done |
