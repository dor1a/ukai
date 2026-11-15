<div align="center">

<img src="internal/web/static/favicon.svg" alt="" width="96">

# ukai

**A small self-hosted web app that downloads YouTube videos and audio with yt-dlp**

Paste a URL or subscribe to a channel, pick video or audio, and the files land in your download directory.

[![build](https://github.com/dor1a/ukai/actions/workflows/build.yml/badge.svg)](https://github.com/dor1a/ukai/actions/workflows/build.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![image](https://img.shields.io/badge/image-ghcr.io%2Fdor1a%2Fukai-2496ED?logo=docker&logoColor=white)](https://github.com/dor1a/ukai/pkgs/container/ukai)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

</div>

> [!NOTE]
> Every phase of [the plan](docs/PLAN.md) is built: downloads with a queue and quality choices, sign-in with OIDC, subscriptions to channels and playlists, the image, and a token API for bots and scripts.

## What it does

- Downloads a URL as video or audio, or several at once with one URL per line (up to 50). Video defaults to the best quality. Audio defaults to the best original stream, kept as it is without re-encoding.
- **Show qualities** asks yt-dlp what the URL offers and lists every resolution and every original audio format. A resolution is a cap: yt-dlp takes the largest at or below it. MP3 at 320, 256, 192 or 128 kbps is always on the list.
- Runs downloads in a queue (`UKAI_CONCURRENCY` at a time). Queued and running jobs can be canceled, and failed or canceled ones tried again with the same settings. The page shows progress while anything is active.
- Lists jobs one per row with the site's thumbnail, uploader, length and size. The list can be searched by title, uploader or URL, narrowed to video or audio and by state, and sorted oldest first.
- Plays a finished download in the page and links each file for saving. Only files a job recorded are served, by their place in that job, from inside the download directory.
- Removes finished jobs from the list one at a time or all at once. The downloaded files stay where they are.
- Subscribes to channels and playlists on the **Subscriptions** page. Each subscription has its own video or audio quality and either downloads everything the channel has and then each new upload, or only what appears after it is added. Its files go to a folder named after it under the download directory, and its own yt-dlp download archive keeps a video from being downloaded twice. Every subscription is checked every `UKAI_SUBS_INTERVAL`, or at once with **Check now**; a check lists only the newest 50 videos, except the first, which lists everything. A channel's main URL is taken as its Videos tab. A subscription can be paused, and unsubscribing keeps the downloaded files.
- Names each file after the title, video ID and quality, so two qualities of one video never share a file: `Title [id] 1920x1080.webm`, `Title [id] 251.opus`, `Title [id] 320k.mp3`.
- Keeps the job history in `jobs.json` in the data directory.
- Signs users in with any standard OIDC provider, or leaves that to a proxy in front. With OIDC, only members of one group get in, and the top bar shows who is signed in.
- Takes downloads from bots and scripts through a small token API, which also reports how each one is going.
- Follows the system's light or dark mode, with a toggle in the top bar.

ukai runs yt-dlp's standalone release binary as a child process. It never imports yt-dlp, so a Python version in the image can never break downloads. It does not use YouTube account cookies.

## Run

```yaml
services:
  ukai:
    image: ghcr.io/dor1a/ukai:latest
    restart: unless-stopped
    environment:
      UKAI_AUTH: none
      TZ: Asia/Seoul
    ports:
      - "8080:8080"
    volumes:
      - ukai-data:/data
      - /path/to/downloads:/downloads

volumes:
  ukai-data:
```

The container runs as uid 10001, so that user needs write access to the downloads directory. Keep the port on a private network.

The image is `linux/amd64` and contains:

| Program | Version | Why |
| -- | -- | -- |
| yt-dlp | 2026.10.26, release binary checked against its SHA-256 | Downloads |
| Deno | 2.9.8, checked the same way | yt-dlp needs it to solve YouTube's JavaScript challenges |
| ffmpeg | 9.0.2, built from the release source checked against its SHA-256 | Merges video and audio, converts to MP3 |

A newer yt-dlp comes with a new image; ukai does not update it inside a running container.

ffmpeg is configured with `--enable-version3` and without the GPL parts, so the build is under the LGPL v3. Its licence files are in `/usr/share/doc/ffmpeg/` in the image, and its source is the release tarball from ffmpeg.org named in the Dockerfile.

### Sign-in

`UKAI_AUTH` has no default, so ukai never starts open by accident. `none` leaves access control to a proxy in front, such as Cloudflare Access. `oidc` signs users in through any standard OIDC provider (Authentik, Keycloak and others work through the same settings):

```yaml
    environment:
      UKAI_AUTH: oidc
      UKAI_PUBLIC_URL: https://ukai.example.com
      UKAI_OIDC_ISSUER: https://id.example.com/application/o/ukai/
      UKAI_OIDC_CLIENT_ID: ukai
      UKAI_OIDC_CLIENT_SECRET: change-me
      UKAI_SESSION_KEY: at-least-32-random-bytes-change-me
```

Register `https://ukai.example.com/oauth/callback` as the redirect URI at the provider, and `https://ukai.example.com/signed-out` as the post-logout redirect URI when the provider supports RP-initiated logout. Only members of `UKAI_OIDC_ALLOWED_GROUP` (default `ukai-users`) may sign in, so the provider must put the user's groups in the ID token. A session lasts 8 hours. The top bar shows who is signed in, with their Gravatar when `UKAI_GRAVATAR=true`.

### API

Scripts and bots queue downloads with a token instead of signing in. Give each caller a name in `UKAI_API_TOKENS` and its token, at least 32 bytes, in `UKAI_API_TOKEN_<NAME>` (upper case, dashes as underscores):

```yaml
      UKAI_API_TOKENS: kizuna
      UKAI_API_TOKEN_KIZUNA: a-long-random-token-change-me-0123456789
```

```sh
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"url": "https://www.youtube.com/watch?v=VIDEO_ID", "kind": "audio", "quality": "mp3-320"}' \
  https://ukai.example.com/api/jobs
curl -H "Authorization: Bearer $TOKEN" https://ukai.example.com/api/jobs/JOB_ID
```

| Request | Answer |
| -- | -- |
| `POST /api/jobs` with `url`, and optionally `kind` (`video`, the default, or `audio`) and `quality` | `202` and the job, with its address in `Location` |
| `GET /api/jobs/{id}` | `200` and the job: `state` (`queued`, `running`, `done`, `failed`, `canceled`), `percent` while running, `files` relative to the download directory when done, `error` when failed |

`quality` is `best` (the default), a height such as `1080` for video, or `mp3-320`, `mp3-256`, `mp3-192` or `mp3-128` for audio. Original audio formats are chosen in the page after a lookup, so the API refuses them. Errors come back as `{"error": "..."}`: `401` without a valid token, `415` when the body is not JSON, `422` for a bad URL, kind or quality, and `404` for a job that left the history. The API works with either `UKAI_AUTH` value; without `UKAI_API_TOKENS` it does not exist.

## Settings

All settings are environment variables.

| Variable | Default | Meaning |
| -- | -- | -- |
| `UKAI_AUTH` | none, required | `oidc` or `none`. ukai refuses to start without it |
| `UKAI_LISTEN` | `:8080` | HTTP listen address |
| `UKAI_DATA_DIR` | `/data` | `jobs.json`, `subs.json`, one download archive per subscription in `archive/`, partial downloads, the yt-dlp and Deno caches (in the image) |
| `UKAI_DOWNLOAD_DIR` | `/downloads` | Where finished files go. It must exist and must not overlap the data directory |
| `UKAI_CONCURRENCY` | `2` | Downloads running at the same time, 1 to 16 |
| `UKAI_JOB_HISTORY` | `200` | Finished jobs kept in `jobs.json`, 1 to 100000 |
| `UKAI_YTDLP` | `/usr/local/bin/yt-dlp` | yt-dlp binary |
| `UKAI_SUBS_INTERVAL` | `6h` | How often every subscription is checked, from `10m` to `168h` |
| `UKAI_PUBLIC_URL` | | Where browsers reach ukai, such as `https://ukai.example.com`, with no path. Required with `oidc` |
| `UKAI_OIDC_ISSUER` | | Issuer URL. Required with `oidc` |
| `UKAI_OIDC_CLIENT_ID` | | Required with `oidc` |
| `UKAI_OIDC_CLIENT_SECRET` | | Required with `oidc` |
| `UKAI_SESSION_KEY` | | Signs the session cookie, at least 32 bytes. Required with `oidc`. Changing it signs everyone out |
| `UKAI_OIDC_ALLOWED_GROUP` | `ukai-users` | Only members of this group may sign in. The name must match the claim exactly; a provider that sends full paths, such as Keycloak, needs the path (`/ukai-users`) |
| `UKAI_OIDC_GROUPS_CLAIM` | `groups` | ID token claim that holds the groups |
| `UKAI_OIDC_SCOPES` | `openid profile email` | Scopes to request. Must include `openid` |
| `UKAI_GRAVATAR` | `false` | `true` shows the signed-in user's Gravatar, found by a SHA-256 hash of the email in the ID token. Browsers then load it from gravatar.com |
| `UKAI_API_TOKENS` | | Names of the API's callers, separated by commas. Empty turns the API off |
| `UKAI_API_TOKEN_<NAME>` | | Each caller's token, at least 32 bytes and different from the others. Required for every name |
| `TZ` | UTC | Time zone of the times on the page, such as `Asia/Seoul`. The binary carries its own time zone data |

`ukai healthcheck` exits 0 when the local server answers `/healthz`. The image uses it as its health check. It stays healthy when `jobs.json` cannot be saved: the jobs page shows a warning instead and ukai keeps trying, because a restart at that point would queue finished jobs again.

ukai logs to stderr, one line per job event (`job queued`, `job started`, `job done`, `job failed`, `job canceled`, `job requeued`, `job removed`) with the job ID and URL, one per subscription event (`subscription added`, `subscription checked`, `subscription ready`, `subscription check failed`, `subscription removed`), one per sign-in (`user signed in`, `user signed out`, `sign-in refused: not in the group`), and one per API submission (`job submitted through the api`, with the token's name).

## Development

The code is Go 1.26 with the standard library, plus `go-oidc` and `oauth2` for sign-in. HTMX is vendored in `internal/web/static`. Tests use a fake yt-dlp script, a fake OIDC provider and made-up IDs, and need no network.

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.26 go test -race ./...
docker build -t ukai .
```

## License

[MIT](LICENSE)
