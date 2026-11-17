# Working rules for pogfarm

Several Claude sessions on different machines work on this repo in parallel. The owner reviews every change on GitHub, so these rules are not optional.

## Git workflow

- The default branch is `master`. Never push to it directly and never merge into it. Only the owner merges.
- One issue, one branch, one pull request. Do not mix unrelated changes.
- Branch name: `<issue-number>-<short-slug>`, for example `9-queue-claim`.
- Before starting an issue, check that it has no `claimed:*` label, then add the label for your machine (`claimed:pc` or `claimed:laptop`). If you don't know which machine you are, ask.
- Do not start an issue whose dependencies (listed in the issue) are not merged yet.
- The pull request body says `Closes #<issue>`, what changed, and how it was tested. Open it and stop; do not merge it.
- Commit messages and pull request descriptions carry no Claude attribution: no `Co-Authored-By` trailer and no "Generated with Claude Code" line.
- Commits use the repo-local git identity (GitHub no-reply email). Never commit with a work email.

## Never commit

Secrets, tokens, real config, the SQLite file, video or audio files. Commit `config.example.yaml`, never `config.yaml`. The repo will be made public later, and git history is permanent.

## Project shape

One repo, one Go module, two binaries.

- `pogfarm` runs once, on the main node: scheduler, chat scanner, job queue (SQLite), Telegram review, uploader.
- `clipper` runs on every node: claims a job, downloads a VOD time range, renders a vertical captioned clip, uploads it to pogfarm. It is stateless and only makes outbound calls to pogfarm.

```
cmd/pogfarm/        main service entry point
cmd/clipper/        render service entry point
internal/api/       job contract shared by both: types, typed client, auth
internal/config/    config file, env, secret files
internal/runner/    run external tools with timeout and captured logs
internal/pogfarm/   store, queue, server, source, scan, review, upload
internal/clipper/   worker, download, layout, transcribe, caption, render
deploy/             Dockerfiles, compose file, Swarm stack file, node setup script
docs/               job contract and architecture notes
```

`internal/pogfarm` and `internal/clipper` must never import each other. They meet only through `internal/api`. A change to the contract updates both sides in the same pull request.

## Design rules

- Platform-specific code (Twitch today) sits behind the source interface in pogfarm and the downloader interface in clipper. Every streamer, VOD and job carries a platform field.
- A job may run twice. pogfarm accepts the first result and ignores later ones.
- Long-running work in pogfarm (scanning, uploading) runs in its own background loop and must not block the bot or the job API.
- External tools (TwitchDownloaderCLI, ffmpeg, the transcriber) are run as subprocesses through `internal/runner`.
- Images are built for both amd64 and arm64.
