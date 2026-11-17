# Architecture

pogfarm is one repo and one Go module (`github.com/salehjafarli/pogfarm`) that builds two binaries.

## The two services

**pogfarm** is the main service. It runs exactly once, on the main node, and owns all state in a single SQLite file. It:

- watches an allowlist of streamers for new VODs
- downloads each VOD's chat, finds activity spikes and queues one render job per moment
- hands jobs to clippers over an HTTP job API, with leases so a job held by a dead node is retried
- sends finished clips to Telegram for approval
- uploads approved clips

**clipper** is the render service. It runs on every node and is the part that scales. It claims a job, downloads that time range of the VOD, renders a vertical captioned clip, and uploads the clip and its transcript to pogfarm. It keeps no state and only ever makes outbound calls to pogfarm, so nodes can join or leave at any time.

They are separate binaries, not one binary with subcommands: their images and dependencies differ, and a clipper node must never be able to start a second pogfarm.

## Layout

```
cmd/pogfarm/        main service entry point
cmd/clipper/        render service entry point
internal/api/       job contract shared by both: types, typed client, auth
internal/config/    config file, env, secret files
internal/runner/    run external tools with timeout and captured logs
internal/pogfarm/   store, queue, server, source, scan, review, upload
internal/clipper/   worker, download, layout, transcribe, caption, render
deploy/             Dockerfiles and compose files
docs/               job contract and architecture notes
```

Each `main.go` only wires packages together and starts the service. All real code lives under `internal/`.

## The import rule

`internal/pogfarm` and `internal/clipper` must never import each other. They meet only through `internal/api`, which holds the job contract. `internal/api`, `internal/config` and `internal/runner` are shared and must not import either service.

This keeps the two services independently buildable and lets work on them proceed in parallel. A change to the contract updates both sides in the same pull request.

## Building

```
make build      # bin/pogfarm and bin/clipper for this machine
make cross      # dist/linux-amd64 and dist/linux-arm64, CGO disabled
make test
make vet
make fmt-check
```

The version printed by each binary is injected at build time with `-ldflags "-X main.version=..."`.

## External tools

### TwitchDownloaderCLI

Pinned version: **1.56.5** (<https://github.com/lay295/TwitchDownloader/releases/tag/1.56.5>). Images and development machines should install this release; check the points below again before moving to a newer one.

pogfarm uses it to fetch the chat of a Twitch VOD (`internal/pogfarm/source/twitch`, `ChatFetcher`). The program is found through `pogfarm.twitch_downloader_path`. One fetch runs:

```
TwitchDownloaderCLI chatdownload --id <vod id> --output <data_dir>/chat-<vod id>-<random>/chat.json --collision Overwrite --banner=false
```

- The `.json` extension of `--output` selects the JSON format.
- Images are embedded only with `-E / --embed-images`, which is not passed.
- `--collision Overwrite` keeps the tool from prompting if the output file exists.
- The work directory is removed when the fetch returns, whatever the outcome. A pogfarm that is killed mid-fetch can leave a `chat-*` directory in the data directory; it is safe to delete.

From the JSON only the `comments` array is read, and of each comment `content_offset_seconds`, `commenter.display_name` (falling back to `commenter.name`), `message.body` and `message.fragments`. A fragment whose `emoticon` is not null is an emote and its `text` is the emote's name. Without embedded images only Twitch's own emotes are marked this way; BTTV, FFZ and 7TV emotes appear in the message text but not in the emote list.

The tool reports a deleted or expired VOD only through the message "Invalid VOD, deleted/expired VOD possibly?"; the fetch turns that into `source.ErrVODNotFound`.

The flags and the JSON layout were checked against the README and source of release 1.56.5. The fetch has not yet been run against the real program: its tests use a stand-in executable and a hand-written file in the same layout.
