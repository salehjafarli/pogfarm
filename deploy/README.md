# deploy

Everything needed to build and run the two services as containers:

- Dockerfiles for the pogfarm image, the clipper base image (ffmpeg, TwitchDownloaderCLI, the transcriber and its models) and the clipper image built on top of it
- the compose file that runs both services on one machine for development
- the standalone clipper compose file for running a clipper on any machine

Images are built for both `linux/amd64` and `linux/arm64`.

No secrets or real config belong here. Compose files read them from the environment or from secret files that are not committed.

## pogfarm image

`deploy/pogfarm.Dockerfile` builds the main service. The image holds the `pogfarm` binary and TwitchDownloaderCLI (for chat downloads) on `debian:13.7-slim`, and nothing else: no ffmpeg, no config, no secrets.

### Build

The build context is the repository root:

```
docker buildx build -f deploy/pogfarm.Dockerfile \
  --platform linux/amd64 \
  --build-arg VERSION=$(git describe --tags --always --dirty) \
  -t pogfarm .
```

Use `--platform linux/arm64` for ARM, or both with `--platform linux/amd64,linux/arm64` when pushing to a registry. The Go binary is cross-compiled on the build machine; only the runtime stage needs QEMU emulation when the target architecture differs from the build machine's. `VERSION` is what `pogfarm -version` prints (default `dev`).

Pinned versions live in build arguments at the top of the Dockerfile: the Go and Debian base images, and `TWITCH_DOWNLOADER_VERSION` with the SHA-256 of its amd64 and arm64 release zips. The version and both checksums change together; the download fails the build if a checksum does not match.

### Run

```
docker volume create pogfarm-data
docker run -d --name pogfarm \
  -v pogfarm-data:/data \
  -v "$PWD/config.yaml:/etc/pogfarm/config.yaml:ro" \
  -e POGFARM_CONFIG=/etc/pogfarm/config.yaml \
  -v "$PWD/secrets/api_secret:/run/secrets/pogfarm_api_secret:ro" \
  -e POGFARM_API_SECRET_FILE=/run/secrets/pogfarm_api_secret \
  -p 8080:8080 \
  pogfarm
```

- **Data.** `/data` is a volume and holds the SQLite database and the clips. Keep it on a named volume or a host directory, or the data is lost with the container. Only one pogfarm may use a data directory at a time.
- **User.** The container runs as user and group `10001` (`pogfarm`), not root. A host directory mounted at `/data` must be writable by UID 10001, for example `sudo chown 10001:10001 <dir>`. A secret or config file mounted into the container must be readable by that user.
- **Port.** The job API listens on `8080` (`POGFARM_LISTEN_ADDR=:8080`). Clippers reach it at `http://<host>:8080`.
- **Config.** No config file is in the image. Without one, everything comes from the defaults and the environment. To use a file, mount it read-only and point `POGFARM_CONFIG` at it (or pass `-config <path>` after the image name). Environment variables win over the file, and the image sets `POGFARM_DATA_DIR=/data`, `POGFARM_LISTEN_ADDR=:8080` and `POGFARM_TWITCH_DOWNLOADER_PATH=/usr/local/bin/TwitchDownloaderCLI`, so the matching keys in a mounted file have no effect; change those with `-e` instead.
- **Secrets.** Every secret can be given as `<VARIABLE>_FILE`, naming a file that holds the value: `POGFARM_API_SECRET_FILE` (required, at least 16 characters), `POGFARM_TWITCH_CLIENT_SECRET_FILE` and `POGFARM_TELEGRAM_BOT_TOKEN_FILE`. This is how Docker and Swarm secrets are mounted (`/run/secrets/<name>`). Plain `-e POGFARM_API_SECRET=...` also works but shows up in `docker inspect`. Setting both a variable and its `_FILE` is an error.

### Health check

The image's `HEALTHCHECK` runs `/usr/local/bin/pogfarm-healthcheck` every 30 seconds (every 2 seconds during the first 30). It sends `GET /healthz` to `127.0.0.1` on the port from `POGFARM_LISTEN_ADDR` using bash's `/dev/tcp`, so the image needs no curl or wget, and is healthy when the answer is `200`, which means the database answers. If `POGFARM_LISTEN_ADDR` is changed to listen on one specific address other than `127.0.0.1`, the check cannot reach it. `docker inspect --format '{{.State.Health.Status}}' pogfarm` shows the state. `/healthz` needs no API secret, and each check appears in the request log.

### Verification

The `image-pogfarm` workflow (`.github/workflows/image-pogfarm.yml`) builds the image for linux/amd64 and linux/arm64 on pull requests and pushes to `master` that touch it, without pushing it anywhere. For each architecture it starts a container with a temporary volume and a generated API secret, waits for it to become healthy, checks that it runs as non-root and that the database was created on the volume, runs TwitchDownloaderCLI, checks that it reaches Twitch's API (a chat request for a missing video must come back as "Invalid VOD"), and writes the image size to the job summary. VODs expire, so no video is fixed in the workflow: to also download real chat, start the workflow by hand with a current VOD ID (`gh workflow run image-pogfarm.yml --ref <branch> -f chat_video_id=<id>`) or set the repository variable `CHAT_VIDEO_ID`.
