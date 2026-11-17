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

## Clipper base image

`clipper-base.Dockerfile` holds the heavy tools the clipper runs. They change rarely, so they live in their own image and the clipper image only adds its Go binary on top. The files it uses are in `clipper-base/`:

| File | Purpose |
| --- | --- |
| `clipper-base/pyproject.toml` | the Python packages, with the CPU-only PyTorch index |
| `clipper-base/uv.lock` | every Python package pinned, with hashes, for both architectures |
| `clipper-base/fetch.sh` | build helper: download a file and check its SHA-256 |
| `clipper-base/detect-faces` | the face detector program, installed as `/opt/pogfarm/bin/detect-faces` |
| `clipper-base/test/` | smoke test and its two small fixtures; not copied into the image |

### What is in it

Both architectures get the same contents, built from the same pins.

| Piece | Version | Where in the image |
| --- | --- | --- |
| Base image | `debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2` (Debian 13) | |
| Debian packages | snapshot.debian.org `20261115T000000Z` (see below) | |
| ffmpeg, ffprobe | Debian `ffmpeg` package from the snapshot, built with libass and libx264 | `/usr/bin` |
| TwitchDownloaderCLI | 1.56.5, official release `Linux-x64` / `LinuxArm64` zip, SHA-256 checked | `/opt/twitchdownloader`, on `PATH` |
| Python | Debian `python3.13` | `/usr/bin/python3.13` |
| Python packages | `uv.lock`: whisperx 3.8.6, torch 2.8.0+cpu, torchaudio 2.8.0, faster-whisper 1.2.1, ctranslate2 4.9.0, pyannote-audio 4.1.0, opencv-python-headless 4.14.0.94 | venv in `/opt/venv`, on `PATH` |
| Whisper model | `small`, CTranslate2 format: `Systran/faster-whisper-small` revision `536b0662742c02347bc0e980a01041f333bce120` | `/models/whisper/faster-whisper-small` (`$POGFARM_WHISPER_MODEL`) |
| Alignment model | torchaudio `WAV2VEC2_ASR_BASE_960H` (WhisperX's English default), `wav2vec2_fairseq_base_ls960_asr_ls960.pth`, SHA-256 checked | `/models/torch/hub/checkpoints` (`$TORCH_HOME`) |
| Sentence splitter | NLTK `punkt_tab`, nltk_data commit `4f15a3d89eefe9748ec1c05be495d91289197155` | `/models/nltk` (`$NLTK_DATA`) |
| Voice activity model | pyannote segmentation model shipped inside the whisperx wheel | in the venv |
| Face detector model | YuNet `face_detection_yunet_2023mar.onnx`, opencv_zoo commit `eb9f4f2d4b79b58e5513b5d25d5f0243a8357bf4` | `/models/yunet` (`$POGFARM_YUNET_MODEL`) |
| Caption font | Montserrat ExtraBold static TTF, JulietaUla/Montserrat commit `555facfb2a18c72c3c0380f0d9c0f060453a9058` | `/usr/local/share/fonts/montserrat`, known to fontconfig |
| uv (build only) | `ghcr.io/astral-sh/uv:0.13.9@sha256:d3b601f0c7b118bc1753febf7c41cc80981df678445e0254284eecf84db79a7d` | not in the final image |

Every downloaded file is checked against a SHA-256 written in the Dockerfile, and every Python wheel against the hash in `uv.lock`.

The image runs as the non-root user `clipper` (uid and gid 10001) with home `/home/clipper`. Nothing in it needs the network to start or to find a model: `HF_HUB_OFFLINE`, `TRANSFORMERS_OFFLINE` and `HF_DATASETS_OFFLINE` are set, `TORCH_HOME` and `NLTK_DATA` point into `/models`, pyannote's telemetry is off (`PYANNOTE_METRICS_ENABLED=false`), and writable caches (`HF_HOME`, `MPLCONFIGDIR`, `XDG_CACHE_HOME`) are under `/tmp`. TwitchDownloaderCLI does need the network when it downloads a VOD, which is its job.

**Debian package pinning.** apt is pointed at a fixed snapshot of the Debian archive (`DEBIAN_SNAPSHOT` in the Dockerfile) instead of pinning each package version. Every package and dependency then comes out the same on each rebuild, and the snapshot never drops old versions the way the live mirrors do. To take Debian updates, move `DEBIAN_SNAPSHOT` to a newer timestamp.

**Left out of the Python environment on purpose** (`override-dependencies` in `pyproject.toml`): `triton`, a GPU compiler that whisperx lists for x86_64 and that is never used on CPU; and `torchcodec`, which pyannote uses only to decode audio files itself. whisperx decodes with the ffmpeg program and hands pyannote a waveform in memory, and there is no torchcodec wheel for linux/aarch64 that matches torch 2.8. Without it pyannote prints a "torchcodec is not installed correctly" warning on import; that warning is expected.

### Running the tools

Transcription (the WhisperX command line; the clipper will run it through `internal/runner`):

```
whisperx clip.wav --model "$POGFARM_WHISPER_MODEL" --model_cache_only True \
  --language en --device cpu --compute_type int8 \
  --output_format json --output_dir /tmp/out
```

Pass the model as a path, as above. Do not pass `--model_dir`: torchaudio would then look for the alignment model there instead of in `$TORCH_HOME`.

Face detection, for webcam detection:

```
detect-faces frame-1.jpg frame-2.jpg
```

It prints one JSON document with, for each image, its size and the faces found as pixel boxes (`x`, `y` top-left, `w`, `h`) with a `confidence`, highest first. Options: `--min-confidence` (default 0.6), `--max-side` (larger images are scaled down to this before detection and boxes scaled back, default 1280), `--model`. The full description is at the top of `clipper-base/detect-faces`.

### Building

Build from the repository root. Build each architecture on a machine of that architecture; under emulation this image takes very long.

```
docker buildx build --platform linux/amd64 -f deploy/clipper-base.Dockerfile -t pogfarm-clipper-base:dev --load .
docker buildx build --platform linux/arm64 -f deploy/clipper-base.Dockerfile -t pogfarm-clipper-base:dev --load .
```

The build downloads about 1.5 GB (Python wheels, models, TwitchDownloaderCLI) and needs BuildKit, which is the default in current Docker.

To change a Python package, edit `clipper-base/pyproject.toml` and run `uv lock` in `deploy/clipper-base`, then commit both files. To change a downloaded file, update its URL and SHA-256 together in the Dockerfile.

### Testing

`clipper-base/test/run-tests.sh` runs inside the built image with networking disabled:

```
docker run --rm --network none -v "$PWD/deploy/clipper-base/test:/test:ro" \
  pogfarm-clipper-base:dev bash /test/run-tests.sh
```

It checks that the process is not root; that ffmpeg is built with libass and libx264 and burns an ASS caption in Montserrat ExtraBold into an H.264 video; that ffprobe reads the sample audio; that TwitchDownloaderCLI starts and reports 1.56.5; that WhisperX transcribes the sample speech with word-level start and end times; and that `detect-faces` finds the one face inside the webcam area of a stream-like frame and no face in an empty frame. It prints the transcript, the first word timestamps, the detector output and the time each step took.

The workflow `.github/workflows/clipper-base.yml` runs this on every pull request that touches the base image files, and on manual dispatch: it builds natively on `ubuntu-24.04` (amd64) and `ubuntu-24.04-arm` (arm64), reports the image size and runs the test. It pushes nothing; publishing the image is a separate workflow. It is not a required check.

Test fixtures:

- `test/jfk.mp3`: 11 seconds of John F. Kennedy's 1961 inaugural address ("ask not what your country can do for you"), a work of the US federal government and so in the public domain. Copied from `samples/jfk.mp3` of whisper.cpp at commit `aba02defd6bb6ff5de63721c21e3456645c153e2`.
- `test/webcam-frame.jpg`: a 1280x720 frame made for this test: flat shapes standing in for a game, with a 320x240 "webcam" in the lower left at x 24 to 344, y 456 to 696. The webcam picture is a crop of NASA photo S69-31741 (portrait of Neil Armstrong, Apollo 11), public domain as a NASA work, taken from Wikimedia Commons `File:Neil_Armstrong_pose.jpg`.

### Transcriber per architecture

WhisperX on both amd64 and arm64. It runs on arm64 with the same lock file (torch 2.8.0 CPU wheels and CTranslate2 both ship linux/aarch64 wheels), so the whisper.cpp fallback is not needed.

### Measured size

Measured by the clipper-base workflow on GitHub's runners (pull request #89), building from scratch with no cache:

| Architecture | Runner | Image size (`docker image inspect`) | Build time | WhisperX on the 11 s sample |
| --- | --- | --- | --- | --- |
| amd64 | `ubuntu-24.04` | 3,635,244,303 bytes (3.64 GB) | 77 s | 12.7 s |
| arm64 | `ubuntu-24.04-arm` | 3,176,236,959 bytes (3.18 GB) | 76 s | 12.2 s |

The largest pieces are the Whisper model (464 MB), the alignment model (361 MB) and PyTorch (718 MB on amd64, 420 MB on arm64). The WhisperX time includes loading the models.

### Licences of what is in the image

| Piece | Licence |
| --- | --- |
| ffmpeg (Debian build) | GPL 2 or later, as built by Debian with libx264 |
| TwitchDownloaderCLI | MIT |
| WhisperX | BSD 2-Clause |
| PyTorch, torchaudio | BSD 3-Clause |
| faster-whisper, CTranslate2 | MIT |
| pyannote.audio and its bundled segmentation model | MIT |
| OpenCV (opencv-python-headless) | Apache 2.0 |
| Whisper `small` weights (OpenAI), converted by Systran | MIT |
| wav2vec 2.0 base 960h (fairseq, via torchaudio) | MIT (weights trained on LibriSpeech, CC BY 4.0) |
| NLTK `punkt_tab` | distributed with NLTK data; NLTK is Apache 2.0 |
| YuNet model | MIT, licence file copied to `/models/yunet/LICENSE` |
| Montserrat font | SIL Open Font License 1.1, licence copied to `/usr/local/share/fonts/montserrat/OFL.txt` |
