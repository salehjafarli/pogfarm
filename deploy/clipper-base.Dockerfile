# Clipper base image: the heavy tools the clipper runs, which change rarely.
# The clipper image (deploy/clipper.Dockerfile) adds only its Go binary on top.
#
# Contents: ffmpeg and ffprobe (with libass and libx264), TwitchDownloaderCLI,
# a Python virtual environment with WhisperX on CPU-only PyTorch, the Whisper
# and English alignment models, the YuNet face detector with its model and the
# detect-faces wrapper, and the Montserrat ExtraBold caption font.
# Every version is pinned; see deploy/README.md for the list and licences.
#
# Build from the repository root, natively for each architecture:
#
#   docker buildx build --platform linux/amd64 -f deploy/clipper-base.Dockerfile \
#     -t pogfarm-clipper-base:dev --load .
#
# Nothing is fetched at run time: models are in /models and the offline
# switches of Hugging Face and PyTorch are set.

# Debian 13 (trixie) slim, multi-architecture index digest.
ARG DEBIAN_IMAGE=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
# Debian packages come from this fixed snapshot of the Debian archive, so a
# rebuild installs exactly the same package versions.
ARG DEBIAN_SNAPSHOT=20261115T000000Z
# uv installs the locked Python environment (deploy/clipper-base/uv.lock).
ARG UV_IMAGE=ghcr.io/astral-sh/uv:0.13.9@sha256:d3b601f0c7b118bc1753febf7c41cc80981df678445e0254284eecf84db79a7d

FROM ${UV_IMAGE} AS uv

# ---------------------------------------------------------------------------
# debian: the base with apt pointed at the snapshot.
FROM ${DEBIAN_IMAGE} AS debian
ARG DEBIAN_SNAPSHOT
RUN set -eu; \
    rm -f /etc/apt/sources.list.d/debian.sources /etc/apt/apt.conf.d/docker-clean; \
    for suite in "debian trixie trixie-updates" "debian-security trixie-security"; do \
      set -- $suite; archive=$1; shift; \
      printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/%s/%s\nSuites: %s\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n\n' \
        "$archive" "$DEBIAN_SNAPSHOT" "$*"; \
    done > /etc/apt/sources.list.d/snapshot.sources; \
    printf '%s\n' \
      'Acquire::Check-Valid-Until "false";' \
      'Acquire::Retries "5";' \
      'Acquire::http::Timeout "60";' \
      'APT::Install-Recommends "false";' \
      'APT::Install-Suggests "false";' \
      'Binary::apt::APT::Keep-Downloaded-Packages "true";' \
      > /etc/apt/apt.conf.d/90pogfarm; \
    cat /etc/apt/sources.list.d/snapshot.sources

# ---------------------------------------------------------------------------
# fetch: downloads TwitchDownloaderCLI, models and the font, each checked
# against a SHA-256 recorded here.
FROM debian AS fetch
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y ca-certificates curl unzip
COPY deploy/clipper-base/fetch.sh /usr/local/bin/fetch

# TwitchDownloaderCLI, self-contained .NET build from the official release.
ARG TARGETARCH
ARG TWITCHDOWNLOADER_VERSION=1.56.5
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) asset=Linux-x64;  sum=f2bcee00d0e6da759d97c23767345634cb9cdb10cdff96cfa96f97983d51bf55 ;; \
      arm64) asset=LinuxArm64; sum=2fde836f07c3ebfcf61cace23742a2c43543fc30fda96071eb96ab8e10c24f7f ;; \
      *) echo "unsupported architecture $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    fetch "https://github.com/lay295/TwitchDownloader/releases/download/${TWITCHDOWNLOADER_VERSION}/TwitchDownloaderCLI-${TWITCHDOWNLOADER_VERSION}-${asset}.zip" \
      "$sum" /tmp/twitchdownloader.zip; \
    mkdir -p /out/opt/twitchdownloader; \
    unzip -q /tmp/twitchdownloader.zip -d /out/opt/twitchdownloader; \
    chmod 0755 /out/opt/twitchdownloader/TwitchDownloaderCLI; \
    ls -la /out/opt/twitchdownloader

# Whisper "small" in CTranslate2 format (faster-whisper), pinned revision of
# https://huggingface.co/Systran/faster-whisper-small. Loaded with
# compute type int8 on CPU.
ARG WHISPER_REPO=Systran/faster-whisper-small
ARG WHISPER_REVISION=536b0662742c02347bc0e980a01041f333bce120
RUN set -eu; \
    base="https://huggingface.co/${WHISPER_REPO}/resolve/${WHISPER_REVISION}"; \
    dir=/out/models/whisper/faster-whisper-small; \
    fetch "$base/config.json"    b55496ac7940a7ae47d2c01eab40edfd8701feec1229d9cce3b40014383fb828 "$dir/config.json"; \
    fetch "$base/tokenizer.json" fb7b63191e9bb045082c79fd742a3106a12c99513ab30df4a0d47fa6cb6fd0ab "$dir/tokenizer.json"; \
    fetch "$base/vocabulary.txt" 34ce3fe1c5041027b3f8d42912270993f986dbc4bb34cf27f951e34a1e453913 "$dir/vocabulary.txt"; \
    fetch "$base/model.bin"      3e305921506d8872816023e4c273e75d2419fb89b24da97b4fe7bce14170d671 "$dir/model.bin"

# WhisperX's default English alignment model, torchaudio's
# WAV2VEC2_ASR_BASE_960H. torchaudio looks for it in $TORCH_HOME/hub/checkpoints.
RUN fetch https://download.pytorch.org/torchaudio/models/wav2vec2_fairseq_base_ls960_asr_ls960.pth \
      488fd4f16de84438ffc945334278c1b9fb9b7159a806c1080b16111a958c945d \
      /out/models/torch/hub/checkpoints/wav2vec2_fairseq_base_ls960_asr_ls960.pth

# NLTK punkt_tab sentence splitter, used by WhisperX's alignment step.
# nltk_data, gh-pages commit 4f15a3d.
RUN set -eu; \
    fetch https://raw.githubusercontent.com/nltk/nltk_data/4f15a3d89eefe9748ec1c05be495d91289197155/packages/tokenizers/punkt_tab.zip \
      e57f64187974277726a3417ca6f181ec5403676c717672eef6a748a7b20e0106 /tmp/punkt_tab.zip; \
    mkdir -p /out/models/nltk/tokenizers; \
    unzip -q /tmp/punkt_tab.zip -d /out/models/nltk/tokenizers

# YuNet face detector (2023mar, fixed input shape for OpenCV 4.x) from
# opencv_zoo commit eb9f4f2. The file is in Git LFS, hence the media URL.
RUN set -eu; \
    base=https://media.githubusercontent.com/media/opencv/opencv_zoo/eb9f4f2d4b79b58e5513b5d25d5f0243a8357bf4/models/face_detection_yunet; \
    fetch "$base/face_detection_yunet_2023mar.onnx" \
      8f2383e4dd3cfbb4553ea8718107fc0423210dc964f9f4280604804ed2552fa4 \
      /out/models/yunet/face_detection_yunet_2023mar.onnx; \
    fetch https://raw.githubusercontent.com/opencv/opencv_zoo/eb9f4f2d4b79b58e5513b5d25d5f0243a8357bf4/models/face_detection_yunet/LICENSE \
      c83b8120c50ccbd4c4f96edf53141bdd566ebb8f8e9227e415326aa1b1aba958 /out/models/yunet/LICENSE

# Caption font: Montserrat ExtraBold (SIL Open Font License 1.1), static TTF
# from JulietaUla/Montserrat commit 555facf.
RUN set -eu; \
    base=https://raw.githubusercontent.com/JulietaUla/Montserrat/555facfb2a18c72c3c0380f0d9c0f060453a9058; \
    fetch "$base/fonts/ttf/Montserrat-ExtraBold.ttf" \
      d3ac6a843d3ba6d5cafd44cf39e437055c8aed7e261010f595f57d3c7b3e2c1b \
      /out/usr/local/share/fonts/montserrat/Montserrat-ExtraBold.ttf; \
    fetch "$base/OFL.txt" \
      8b7141c03fa4f8d44e6345d5d4931709290f0f67875e452e95ac1fd3a027802e \
      /out/usr/local/share/fonts/montserrat/OFL.txt

# ---------------------------------------------------------------------------
# python: the virtual environment, installed from the committed uv.lock.
# uv checks every wheel against the hash in the lock.
FROM debian AS python
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y python3.13
COPY --from=uv /uv /usr/local/bin/uv
ENV UV_PROJECT_ENVIRONMENT=/opt/venv \
    UV_PYTHON=/usr/bin/python3.13 \
    UV_PYTHON_DOWNLOADS=never \
    UV_LINK_MODE=copy \
    UV_COMPILE_BYTECODE=1
WORKDIR /src
COPY deploy/clipper-base/pyproject.toml deploy/clipper-base/uv.lock ./
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-dev --no-install-project

# ---------------------------------------------------------------------------
# final: runtime pieces only.
FROM debian AS final
# ffmpeg/ffprobe (Debian's build has libass and libx264), Python for the
# venv, fontconfig so libass finds the caption font, and the libraries the
# self-contained .NET TwitchDownloaderCLI needs (ICU, OpenSSL, CA roots).
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y \
      ca-certificates \
      ffmpeg \
      fontconfig \
      libicu76 \
      libssl3t64 \
      python3.13 \
    && ffmpeg -hide_banner -version | head -n 1

RUN groupadd --system --gid 10001 clipper \
    && useradd --system --uid 10001 --gid clipper --create-home --home-dir /home/clipper --shell /usr/sbin/nologin clipper

COPY --from=fetch /out/ /
COPY --from=python /opt/venv /opt/venv
COPY deploy/clipper-base/detect-faces /opt/pogfarm/bin/detect-faces
RUN chmod 0755 /opt/pogfarm/bin/detect-faces \
    && ln -s /opt/twitchdownloader/TwitchDownloaderCLI /usr/local/bin/TwitchDownloaderCLI \
    && fc-cache -f \
    && fc-match 'Montserrat:extrabold'

# Models live in the image: the offline switches stop Hugging Face, PyTorch
# and pyannote from reaching for the network. Writable caches go to /tmp,
# since the image itself is read-only to the clipper user.
ENV PATH=/opt/pogfarm/bin:/opt/venv/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    HF_HUB_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1 \
    HF_DATASETS_OFFLINE=1 \
    HF_HUB_DISABLE_TELEMETRY=1 \
    PYANNOTE_METRICS_ENABLED=false \
    TORCH_HOME=/models/torch \
    NLTK_DATA=/models/nltk \
    POGFARM_WHISPER_MODEL=/models/whisper/faster-whisper-small \
    POGFARM_YUNET_MODEL=/models/yunet/face_detection_yunet_2023mar.onnx \
    HF_HOME=/tmp/huggingface \
    MPLCONFIGDIR=/tmp/matplotlib \
    XDG_CACHE_HOME=/tmp/cache \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    DOTNET_CLI_TELEMETRY_OPTOUT=1

USER clipper:clipper
WORKDIR /home/clipper
