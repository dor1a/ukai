# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ukai ./cmd/ukai

# yt-dlp and Deno are release binaries, and ffmpeg is a source release, all
# pinned by version and SHA-256. yt-dlp needs Deno to solve YouTube's
# JavaScript challenges.
FROM --platform=$BUILDPLATFORM debian:trixie-slim AS tools
ARG TARGETARCH
ARG YTDLP_VERSION=2026.10.26
ARG YTDLP_SHA256_AMD64=767ba9dce7052aa196f8b12652f433d76de128ba168966fa84b6394d5e6c6795
ARG YTDLP_SHA256_ARM64=e87e9533e89fe5889abaa21f5114dfe7756d6aa6219b83551add04d48b18e483
ARG DENO_VERSION=v2.9.8
ARG DENO_SHA256_AMD64=e890975bc2549807bbcc0b584ffafda72cc7c93349dd99c80483dd784f6c4609
ARG DENO_SHA256_ARM64=d49596c20b9788cf8f5d8ddf644103d4bde56ed239753f0bdb92ba9361d51905
ARG FFMPEG_VERSION=9.0.2
ARG FFMPEG_SHA256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip xz-utils \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /out
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) y=yt-dlp_linux; ys=$YTDLP_SHA256_AMD64; d=deno-x86_64-unknown-linux-gnu.zip; ds=$DENO_SHA256_AMD64 ;; \
      arm64) y=yt-dlp_linux_aarch64; ys=$YTDLP_SHA256_ARM64; d=deno-aarch64-unknown-linux-gnu.zip; ds=$DENO_SHA256_ARM64 ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o yt-dlp "https://github.com/yt-dlp/yt-dlp/releases/download/$YTDLP_VERSION/$y"; \
    echo "$ys  yt-dlp" | sha256sum -c -; \
    curl -fsSL -o deno.zip "https://github.com/denoland/deno/releases/download/$DENO_VERSION/$d"; \
    echo "$ds  deno.zip" | sha256sum -c -; \
    unzip -q deno.zip deno; \
    rm deno.zip; \
    chmod 0755 yt-dlp deno
RUN set -eu; \
    curl -fsSL -o /tmp/ffmpeg.tar.xz "https://ffmpeg.org/releases/ffmpeg-$FFMPEG_VERSION.tar.xz"; \
    echo "$FFMPEG_SHA256  /tmp/ffmpeg.tar.xz" | sha256sum -c -; \
    mkdir /ffmpeg-src; \
    tar -xJf /tmp/ffmpeg.tar.xz -C /ffmpeg-src --strip-components=1; \
    rm /tmp/ffmpeg.tar.xz

# Debian's ffmpeg package brings about 400MB of libraries, Mesa and LLVM among
# them. This build has FFmpeg's own formats and codecs, LAME for MP3, and
# OpenSSL for HTTPS, which yt-dlp needs when it hands a live stream to ffmpeg.
# It runs on the target platform and links against the runtime's Debian
# libraries.
FROM debian:trixie-slim AS ffmpeg
RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev make nasm pkgconf \
       libmp3lame-dev libssl-dev zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*
COPY --from=tools /ffmpeg-src /src
WORKDIR /src
RUN ./configure --disable-autodetect --disable-debug --disable-doc --disable-ffplay \
      --enable-version3 --enable-iconv --enable-libmp3lame --enable-openssl --enable-zlib \
    && make -j"$(nproc)" ffmpeg ffprobe

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libmp3lame0 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=tools /out/yt-dlp /out/deno /usr/local/bin/
COPY --from=ffmpeg /src/ffmpeg /src/ffprobe /usr/local/bin/
# The build is LGPL v3; its licence goes with the binaries.
COPY --from=ffmpeg /src/LICENSE.md /src/COPYING.LGPLv3 /usr/share/doc/ffmpeg/
COPY --from=build /out/ukai /usr/local/bin/ukai
RUN useradd --system --uid 10001 --no-create-home ukai \
    && install -d -o ukai -g ukai /data /downloads
USER ukai
# yt-dlp and Deno keep their caches under the data directory.
ENV UKAI_DATA_DIR=/data \
    UKAI_DOWNLOAD_DIR=/downloads \
    XDG_CACHE_HOME=/data/cache
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["ukai", "healthcheck"]
ENTRYPOINT ["ukai"]
CMD ["serve"]
