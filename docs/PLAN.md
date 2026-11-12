# ukai plan

> Status: phase 1 is built, with the image and the GitHub Actions build from phase 4 (amd64 only). Phases 2 to 5 are otherwise plans. Values marked `draft` are starting points and may change.

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
| `UKAI_SUBS_INTERVAL` | `6h` (draft) | how often subscriptions are checked |
| `UKAI_YTDLP` | `/usr/local/bin/yt-dlp` | yt-dlp binary |
| `UKAI_OIDC_ISSUER` | | issuer URL, required with `oidc` |
| `UKAI_OIDC_CLIENT_ID` | | required with `oidc` |
| `UKAI_OIDC_CLIENT_SECRET` | | required with `oidc` |
| `UKAI_OIDC_ALLOWED_GROUP` | empty | if set, only members of this group may use ukai |
| `UKAI_OIDC_GROUPS_CLAIM` | `groups` | claim that holds the groups |
| `UKAI_OIDC_SCOPES` | `openid profile email` | scopes to request |
| `UKAI_SESSION_SECRET` | | key that signs the session cookie, required with `oidc` |

## Phases

### 1. Download one URL

- Submit a URL with a choice of video or audio. Video defaults to the best quality. Audio defaults to the best original stream, kept as it is without re-encoding.
- Other qualities are choices. Before a lookup, video offers Best only and audio offers Best plus MP3 at 320, 256, 192 or 128 kbps. "Show qualities" asks yt-dlp for the formats of that URL (`--dump-single-json`) and adds every height the video has (a cap: yt-dlp takes the largest at or below it) and every original audio format. Video sizes are counted by the shorter side, as yt-dlp's `res` and YouTube do, so a portrait 1080x1920 video is 1080p. At most two lookups run at once, each for at most 90 seconds.
- An original audio format belongs to the URL it was looked up for. A download with that format and a different URL is refused.
- File names carry the quality, so two qualities of one video never share a file: `Title [id] 1920x1080.webm`, `Title [id] 251.opus` (format ID), `Title [id] 320k.mp3`.
- A queue with `UKAI_CONCURRENCY` workers. Jobs can be cancelled.
- One yt-dlp run per job. Metadata comes from `--print before_dl:` lines and the final path from `--print after_move:`, progress from `--progress-template` lines. The jobs page shows them with HTMX polling while anything is queued or running.
- `store` behind an interface, backed by JSON files written atomically. Only the last `UKAI_JOB_HISTORY` finished jobs are kept.
- `/healthz` for the container health check.
- Tests run against a fake yt-dlp script that prints recorded metadata and progress lines.

### 2. Sign-in

- Port `internal/auth` from noroshi: discovery, authorization code with PKCE, signed session cookie, sign-out (also at the provider when it supports RP-initiated logout).
- `UKAI_OIDC_ALLOWED_GROUP` replaces noroshi's admin group. Empty means any signed-in user.
- Startup check: `UKAI_AUTH` missing or unknown, or `oidc` without its required settings, stops ukai with a message that names the missing variable.

### 3. Subscriptions

- Add a channel or artist URL with one of two modes, chosen per subscription:
  - `backfill`: download everything the channel has, then keep up with new uploads.
  - `new-only`: download only what appears after the subscription is added.
- Each subscription has its own yt-dlp download archive, so items are never downloaded twice.
- For `new-only`, the existing items are written to the archive when the subscription is added, without downloading them. yt-dlp's `--skip-download --force-write-archive` looks like the way to do this; to be confirmed in this phase.
- Checks run every `UKAI_SUBS_INTERVAL` and can be started by hand.

### 4. Image and deployment

- Multi-stage Dockerfile: build the Go binary, then a runtime image with ffmpeg, Deno and the yt-dlp release binary. The yt-dlp version is pinned and its file is checked against the SHA-256 sums published with the release.
- GitHub Actions: tests on every push, image to `ghcr.io/dor1a/ukai` on tags. Built for amd64 only: the runtime stage installs ffmpeg with apt, which needs QEMU for arm64.
- Compose example for one host. The port stays on the internal network; outside access goes through the proxy in front.

### 5. API for bots and scripts

- A token-protected API to submit a job (`POST /api/jobs`), for a Discord bot or scripts on the same private network.
- The token is separate from OIDC.

## Out of scope

- Account cookies and Premium-only formats
- Renaming, tagging and moving files into a media library
- Accounts, quotas or per-user history inside ukai
- Promising support for sites other than YouTube. yt-dlp may handle them, but ukai is tested against YouTube only

## Branches and releases

- `dev` is the default branch. `main` is created at the first release and follows releases.
- Releases are tags `vX.Y.Z`. Each tag builds an image.

## Open items

| Item | Notes |
| -- | -- |
| How to update yt-dlp | Rebuild the image with a new pinned version, or allow an update inside the container. Rebuilding keeps the checksum check |
| `UKAI_SUBS_INTERVAL` default | 6 hours is a guess |
