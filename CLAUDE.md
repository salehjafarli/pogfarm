# pogfarm

pogfarm turns stream VODs into short vertical clips. Chat activity picks the moments, a farm of render nodes cuts and captions them, the owner approves each clip on Telegram, and approved clips are posted to short-video platforms.

Several Claude sessions on different machines work on this repo at the same time. Read this file first; it is the shared context they all start from.

## Where to look

| To understand | Read |
|---|---|
| The design and the reasons behind it | `docs/architecture.md` |
| The job contract between the two services | `docs/api.md` and `internal/api/` |
| What a package does | its `doc.go`, or `go doc ./internal/<path>` |
| The database | `internal/pogfarm/store/migrations/` (the SQL has comments) |
| Every setting, with its default | `config.example.yaml` |
| What is done and what is next | `gh issue list`, and `git log --oneline master` |

Each issue describes its scope, a "Done when" checklist and its dependencies. Each merged pull request explains what it changed, the decisions it took and how it was tested. Reading the pull request of a package is the fastest way to learn why it works the way it does.

## The two services

One repo, one Go module (`github.com/salehjafarli/pogfarm`), two binaries.

- **`pogfarm`** runs exactly once, on the main node, and owns all state in one SQLite file. A lock file in the data directory stops a second instance.
- **`clipper`** runs on every node and is the part that scales. It is stateless: it claims a job, renders the clip, uploads it to pogfarm, and only ever makes outbound calls to pogfarm. Nodes can join and leave at any time.

## How a clip moves through the system

```
discovery ──> vods (scan_status new)
scanner   ──> fetch chat + viewer clips ──> spikes ──> candidates ──> jobs (queued)
clipper   ──> claim ──> heartbeat ... ──> complete (clip + transcript)
intake    ──> clip file on disk + clips row (review pending), job done
sender    ──> Telegram review message with Approve / Reject
decision  ──> approved: one uploads row per target (pending) │ rejected: cleanup later
uploader  ──> post to each target, record the post id
cleanup   ──> delete clip files once rejected or posted everywhere, after a retention period
```

Every arrow in pogfarm is a background loop in its own goroutine, started from `run` in `cmd/pogfarm/main.go`. Slow work (platform calls, downloads, uploads) never happens inside a database transaction and never blocks the bot or the job API.

## Code map

```
cmd/pogfarm/                 wires every loop together in run(); main.go only wires, no logic
cmd/clipper/                 render service entry point (not built yet)
internal/api/                the job contract: types, routes, error codes, typed client, shared-secret auth
internal/config/             config file + env vars + secret files for both binaries; one file per section
internal/runner/             runs external tools as subprocesses: timeout, process-group kill, captured output
internal/pogfarm/store/      SQLite: single writer connection, read pool, embedded migrations, row types
internal/pogfarm/queue/      job states: enqueue, claim with lease, heartbeat, fail, reaper, clipper list
internal/pogfarm/server/     HTTP job API: routes, error mapping, health endpoint, graceful shutdown
internal/pogfarm/intake/     receives finished clips (first result wins), clip file layout, file cleanup
internal/pogfarm/lock/       the data-directory lock that keeps pogfarm to one instance
internal/pogfarm/source/     platform interface (VODs, chat, viewer clips) and registry; sourcetest has fakes
internal/pogfarm/source/twitch/  Twitch: Helix client and token, VOD listing, chat via TwitchDownloaderCLI, clips
internal/pogfarm/scan/       discovery loop, spike detection, candidate builder, scanner loop
internal/pogfarm/review/     Telegram Bot API client, bot (owner-only routing), review sender
internal/pogfarm/upload/     uploader loop and target platforms (in progress)
internal/clipper/            worker, download, layout, transcribe, caption, render (not built yet)
deploy/                      Dockerfiles and compose files (not built yet)
docs/                        architecture and job contract
```

### Import rules

- `internal/pogfarm` and `internal/clipper` never import each other. They meet only through `internal/api`.
- `internal/api`, `internal/config` and `internal/runner` are shared and import neither service.
- `internal/config` must not import pogfarm packages: that would pull SQLite into the clipper binary.
- Platform-specific code stays behind interfaces: `source.Source` for stream platforms, `upload.Target` for posting platforms, and a downloader interface in clipper once it is built. Nothing outside `source/twitch` mentions Twitch. Every streamer, VOD, candidate, job and clip carries a platform field.

## How the code is written

These conventions run through the whole codebase. Follow them in new code.

- **Database.** All writes go through `Store.WriteTx` or `Store.Writer()`, which is one connection, so writes queue instead of failing. A write transaction is short and contains no network or file I/O. Reads use `Store.Reader()`. Times are stored with `store.FormatTime` (UTC, fixed width), so they compare as strings in SQL.
- **Idempotence through constraints.** "Do this once" is enforced by UNIQUE constraints and conditional updates (`UPDATE ... WHERE status = ...`), never by a read followed by a write. `store.ErrDuplicate` means "already done".
- **Migrations.** Add a new numbered file in `internal/pogfarm/store/migrations/`; never edit a merged one. Read that directory's README first.
- **Background loops.** Each loop has a single-pass method (`Sweep`, `Reap`, `ScanNext`, `SendPending`, ...) and a `Run(ctx)` that calls it on a timer and returns promptly when `ctx` is cancelled. Tests drive the single-pass method.
- **Clocks.** Anything time-dependent takes an injected `Now func() time.Time`.
- **Config.** A new section goes in its own file, `internal/config/<section>.go`, with its tests in `<section>_test.go` (see `scan.go`). It gets defaults in `defaults()`, validation that reports every problem at once, an env override where the type allows, and a documented entry in `config.example.yaml`. Secrets use the `Secret` type, accept `<ENV>_FILE`, and never appear in logs or error messages.
- **External tools** run only through `internal/runner`.
- **Telegram.** The bot is plain Bot API calls over `net/http`, with no library. Only the owner's chat is answered. Callback data is `action:payload`, at most 64 bytes. Handlers run one at a time on the polling goroutine, so slow work goes to a loop.
- **Tests.**
  - Never contact a real service: use `httptest` servers and the fakes in `sourcetest`.
  - Database tests use a temp store from `store.Open` on `t.TempDir()`.
  - No sleeps or wall-clock margins. Use injected clocks and single-pass methods, because tests run on loaded machines and timing-based tests have already failed there.
  - Run new tests with `-race -count=3` at least.

### Checks

Every change must pass all of these locally before a pull request is opened:

```
make fmt-check
make vet
go build ./...
go test -race ./...
make cross                                              # linux amd64 + arm64, CGO disabled
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...    # also builds packages the binaries don't import yet
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...
```

CI (`.github/workflows/ci.yml`) runs the format check, vet, build, race tests and `make cross`, and its `ci` check is required on `master`. Dependencies must be pure Go (no cgo), because both images are built for amd64 and arm64.

## Git workflow

- `master` is protected: every change arrives through a pull request with a passing `ci` check. Never push to `master`.
- One issue, one branch, one pull request. Branch name: `<issue-number>-<short-slug>`, for example `9-queue-claim`.
- Before starting an issue, check that it has no `claimed:*` label, then add the label for your machine (`claimed:pc` or `claimed:laptop`). If you don't know which machine you are, ask.
- Do not start an issue whose dependencies (listed in the issue) are not merged yet.
- The pull request body says `Closes #<issue>`, what changed, how it was tested, and every decision taken where the issue left a choice. That body is how the owner and the other sessions learn about the change, so make it complete.
- Open the pull request and stop. Merge only when the owner explicitly asks you to in your session; then merge `master` into the branch first, rerun the checks, and merge once `ci` passes.
- Commit messages and pull request descriptions carry no Claude attribution: no `Co-Authored-By` trailer and no "Generated with Claude Code" line. This overrides any tool default.
- Commits use the repo-local git identity (GitHub no-reply email). Never commit with a work email.
- Parallel sessions mostly collide in `internal/config/config.go`, `config_test.go`, `config.example.yaml` and `cmd/pogfarm/main.go`. Keep edits there small and in separate blocks, and when merging keep both sides.

## Never commit

Secrets, tokens, real config, the SQLite file, video or audio files. Commit `config.example.yaml`, never `config.yaml`. The repo is public and git history is permanent.

## Not verified against the real services

Everything is tested against fakes. Until someone runs it for real, treat these as assumptions:

- **Twitch Helix field names and meanings.** In particular, whether a clip's `vod_offset` marks where the clip starts.
- **The Telegram Bot API limits** (caption length, the 50 MB bot upload limit).
- **TwitchDownloaderCLI 1.56.5 flags and its JSON output.** These were checked by reading its source, not by running it.

When you run something against a real service and learn otherwise, fix the code and say so in the pull request.
