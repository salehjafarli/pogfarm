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

## Status

Design stage. Twitch is the only source for now; source-specific code sits behind one interface so other platforms can be added.
