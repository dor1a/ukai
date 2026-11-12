<div align="center">

<img src="internal/web/static/favicon.svg" alt="" width="96">

# ukai

**A small self-hosted web app that downloads YouTube videos and audio with yt-dlp**

Paste a URL, pick video or audio, and the file lands in one directory.

[![build](https://github.com/dor1a/ukai/actions/workflows/build.yml/badge.svg)](https://github.com/dor1a/ukai/actions/workflows/build.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![image](https://img.shields.io/badge/image-ghcr.io%2Fdor1a%2Fukai-2496ED?logo=docker&logoColor=white)](https://github.com/dor1a/ukai/pkgs/container/ukai)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

</div>

> [!NOTE]
> Phase 1 of [the plan](docs/PLAN.md) is built: one URL at a time, a job queue and the quality choices. Sign-in (OIDC) is phase 2, so for now ukai runs with `UKAI_AUTH=none` behind a proxy that controls access, such as Cloudflare Access. Subscriptions to channels come in phase 3.

## What it does

- Downloads one URL as video or audio. Video defaults to the best quality. Audio defaults to the best original stream, kept as it is without re-encoding.
- **Show qualities** asks yt-dlp what the URL offers and lists every resolution and every original audio format. A resolution is a cap: yt-dlp takes the largest at or below it. MP3 at 320, 256, 192 or 128 kbps is always on the list.
- Runs downloads in a queue (`UKAI_CONCURRENCY` at a time). Queued and running jobs can be canceled. The page shows progress while anything is active.
- Names each file after the title, video ID and quality, so two qualities of one video never share a file: `Title [id] 1920x1080.webm`, `Title [id] 251.opus`, `Title [id] 320k.mp3`.
- Keeps the job history in `jobs.json` in the data directory.

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

The container runs as uid 10001, so that user needs write access to the downloads directory. Keep the port on a private network and put the access control in front.

The image is `linux/amd64` and contains:

| Program | Version | Why |
| -- | -- | -- |
| yt-dlp | 2026.10.26, release binary checked against its SHA-256 | Downloads |
| Deno | 2.9.8, checked the same way | yt-dlp needs it to solve YouTube's JavaScript challenges |
| ffmpeg | 9.0.2, built from the release source checked against its SHA-256 | Merges video and audio, converts to MP3 |

A newer yt-dlp comes with a new image.

ffmpeg is configured with `--enable-version3` and without the GPL parts, so the build is under the LGPL v3. Its licence files are in `/usr/share/doc/ffmpeg/` in the image, and its source is the release tarball from ffmpeg.org named in the Dockerfile.

## Settings

All settings are environment variables.

| Variable | Default | Meaning |
| -- | -- | -- |
| `UKAI_AUTH` | none, required | `none` for now. `oidc` comes in phase 2. ukai refuses to start without it |
| `UKAI_LISTEN` | `:8080` | HTTP listen address |
| `UKAI_DATA_DIR` | `/data` | `jobs.json`, partial downloads, the yt-dlp and Deno caches (in the image) |
| `UKAI_DOWNLOAD_DIR` | `/downloads` | Where finished files go. It must exist and must not overlap the data directory |
| `UKAI_CONCURRENCY` | `2` | Downloads running at the same time, 1 to 16 |
| `UKAI_JOB_HISTORY` | `200` | Finished jobs kept in `jobs.json`, 1 to 100000 |
| `UKAI_YTDLP` | `/usr/local/bin/yt-dlp` | yt-dlp binary |
| `TZ` | UTC | Time zone of the times on the page, such as `Asia/Seoul`. The binary carries its own time zone data |

`ukai healthcheck` exits 0 when the local server answers `/healthz`. The image uses it as its health check. It stays healthy when `jobs.json` cannot be saved: the jobs page shows a warning instead and ukai keeps trying, because a restart at that point would queue finished jobs again.

ukai logs to stderr, one line per job event (`job queued`, `job started`, `job done`, `job failed`, `job canceled`, `job requeued`) with the job ID and URL.

## Development

The code is Go 1.26 with the standard library only. HTMX is vendored in `internal/web/static`. Tests use a fake yt-dlp script and made-up IDs, and need no network.

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.26 go test -race ./...
docker build -t ukai .
```

## License

[MIT](LICENSE)
