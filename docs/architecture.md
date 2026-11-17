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
