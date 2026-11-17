# syntax=docker/dockerfile:1

# Image of the main service, pogfarm: the pogfarm binary plus
# TwitchDownloaderCLI, which pogfarm runs to download VOD chat.
#
# Build from the repository root (the build context must be the root):
#
#   docker buildx build -f deploy/pogfarm.Dockerfile --platform linux/amd64 \
#     --build-arg VERSION=$(git describe --tags --always --dirty) -t pogfarm .
#
# Supported platforms: linux/amd64 and linux/arm64. The Go stage always runs
# on the build machine's own platform and cross-compiles, so only the small
# runtime stage needs emulation when building for another architecture.
#
# See deploy/README.md for how to run it.

# Versions of the base images. Change them here only.
ARG GO_IMAGE=golang:1.27.4-trixie
ARG DEBIAN_IMAGE=debian:13.7-slim

# TwitchDownloaderCLI release and the SHA-256 of its two Linux assets.
# The three values belong together: when moving to a new release, change the
# version and both checksums in the same commit, and update the pinned
# version in docs/architecture.md. The checksums come from the release page
# (each asset's "sha256:" digest, also in
# `gh api repos/lay295/TwitchDownloader/releases/tags/<version>`) and should
# be confirmed by downloading the two zips and running `sha256sum` on them.
#   amd64: TwitchDownloaderCLI-<version>-Linux-x64.zip
#   arm64: TwitchDownloaderCLI-<version>-LinuxArm64.zip
ARG TWITCH_DOWNLOADER_VERSION=1.56.5
ARG TWITCH_DOWNLOADER_SHA256_AMD64=f2bcee00d0e6da759d97c23767345634cb9cdb10cdff96cfa96f97983d51bf55
ARG TWITCH_DOWNLOADER_SHA256_ARM64=2fde836f07c3ebfcf61cace23742a2c43543fc30fda96071eb96ab8e10c24f7f

# ---------------------------------------------------------------------------
# Go build: native on the build machine, cross-compiled for the target.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src

# Modules first, so this layer is reused until go.mod or go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/pogfarm ./cmd/pogfarm

# ---------------------------------------------------------------------------
# TwitchDownloaderCLI: download the release asset for the target architecture
# and check its checksum. Runs on the build machine; nothing here is executed
# for the target.
FROM --platform=$BUILDPLATFORM ${DEBIAN_IMAGE} AS twitch-downloader
ARG TARGETARCH
ARG TWITCH_DOWNLOADER_VERSION
ARG TWITCH_DOWNLOADER_SHA256_AMD64
ARG TWITCH_DOWNLOADER_SHA256_ARM64
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) asset=Linux-x64;  sum="$TWITCH_DOWNLOADER_SHA256_AMD64" ;; \
      arm64) asset=LinuxArm64; sum="$TWITCH_DOWNLOADER_SHA256_ARM64" ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    url="https://github.com/lay295/TwitchDownloader/releases/download/${TWITCH_DOWNLOADER_VERSION}/TwitchDownloaderCLI-${TWITCH_DOWNLOADER_VERSION}-${asset}.zip"; \
    curl -fsSL --retry 3 -o /tmp/tdcli.zip "$url"; \
    echo "${sum}  /tmp/tdcli.zip" | sha256sum -c -; \
    mkdir -p /out; \
    unzip -q /tmp/tdcli.zip TwitchDownloaderCLI COPYRIGHT.txt THIRD-PARTY-LICENSES.txt -d /out; \
    chmod 0755 /out/TwitchDownloaderCLI

# ---------------------------------------------------------------------------
# Runtime.
#
# TwitchDownloaderCLI is a self-contained, single-file .NET program built for
# glibc, so the base must be glibc-based (not Alpine or distroless/static) and
# must provide what .NET loads at run time: libicu for globalization, libssl
# for HTTPS, libstdc++ and CA certificates.
FROM ${DEBIAN_IMAGE}
ARG TWITCH_DOWNLOADER_VERSION
ARG VERSION=dev

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates tzdata libicu76 libssl3t64 libstdc++6 \
    && rm -rf /var/lib/apt/lists/*

# Fixed IDs so the owner of files on the data volume is predictable.
RUN groupadd --system --gid 10001 pogfarm \
    && useradd --system --uid 10001 --gid pogfarm --home-dir /nonexistent --no-create-home \
        --shell /usr/sbin/nologin pogfarm \
    && mkdir -p /data \
    && chown pogfarm:pogfarm /data \
    && chmod 0750 /data

COPY --from=twitch-downloader /out/TwitchDownloaderCLI /usr/local/bin/TwitchDownloaderCLI
COPY --from=twitch-downloader /out/COPYRIGHT.txt /out/THIRD-PARTY-LICENSES.txt /usr/share/doc/twitch-downloader/
COPY --chmod=0755 deploy/pogfarm-healthcheck.sh /usr/local/bin/pogfarm-healthcheck
COPY --from=build /out/pogfarm /usr/local/bin/pogfarm

# Settings that belong to the image. Everything else, including the API
# secret, comes from a mounted config file (POGFARM_CONFIG) or from the
# environment at run time. Note that environment variables win over the
# config file, so data_dir and listen_addr in a mounted file are overridden
# by these unless the variables are changed too.
ENV POGFARM_DATA_DIR=/data \
    POGFARM_LISTEN_ADDR=:8080 \
    POGFARM_TWITCH_DOWNLOADER_PATH=/usr/local/bin/TwitchDownloaderCLI \
    DOTNET_BUNDLE_EXTRACT_BASE_DIR=/tmp/dotnet-bundle

LABEL org.opencontainers.image.title="pogfarm" \
      org.opencontainers.image.description="pogfarm main service: scheduler, chat scanner, job queue, review and uploader" \
      org.opencontainers.image.source="https://github.com/salehjafarli/pogfarm" \
      org.opencontainers.image.version="${VERSION}" \
      io.github.lay295.twitchdownloader.version="${TWITCH_DOWNLOADER_VERSION}"

USER pogfarm:pogfarm
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --start-interval=2s --retries=3 \
    CMD ["/usr/local/bin/pogfarm-healthcheck"]

ENTRYPOINT ["/usr/local/bin/pogfarm"]
