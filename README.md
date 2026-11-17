# pogfarm

A distributed pipeline that turns stream VODs into short vertical clips, with a human review step before anything is posted.

## Architecture

Two services, both written in Go and run as containers on a Docker Swarm cluster.

**pogfarm** runs once, on the main node. It does everything that is light and must not be duplicated:

- watches an allowlist of streamers for new VODs
- downloads each VOD's chat, finds activity spikes and queues one render job per moment
- hands jobs to clippers through an HTTP job API, with leases so a job held by a dead node is retried
- sends finished clips to Telegram for approval
- uploads approved clips

State lives in a single SQLite file owned by this service.

**clipper** runs on every node and is the part that scales. It loops:

1. claim a job (platform, VOD, start, end)
2. download that time range
3. reformat to vertical, transcribe, burn in captions
4. upload the finished clip and its transcript to pogfarm

Clippers are stateless and only ever make outbound calls to pogfarm, so nodes can join or leave at any time.

## Content and permissions

pogfarm only touches content from streamers it has been told about, and only when a reason for permission is on record.

- **Allowlist only.** The scheduler works from an explicit allowlist of streamers in the config. It never discovers or scans streamers on its own.
- **A permission note per streamer.** Every allowlist entry must record where permission to clip came from: a paid clipping program, a published clip policy, or written permission from the streamer. An entry without a permission note is rejected when the config is loaded.
- **Human review.** Every clip is sent to a person for approval before it is posted. Nothing is published automatically.
- **Credit.** Posts credit the streamer the clip came from.
- **Your responsibility.** The software does not check or grant rights. Whoever runs it is responsible for having the rights to what they post, including anything in a clip that the streamer does not own, such as music.

## Status

Early development. The design is settled and the work is tracked in the repository's issues; nothing runs end to end yet. Twitch is the only source for now; source-specific code sits behind one interface so other platforms can be added.
