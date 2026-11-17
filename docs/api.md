# Job API contract

This is the only interface between pogfarm and clipper. The Go types live in `internal/api`; this document is the written form of the same contract. A change to one must change the other, and both services, in the same pull request.

## Basics

- **Direction:** clippers call pogfarm. pogfarm never calls a clipper.
- **Transport:** HTTP with JSON bodies, except the complete operation, which is a multipart upload.
- **Version:** every route is under `/v1`.
- **Times** are RFC 3339 in UTC, for example `2026-11-17T14:32:00Z`.
- **Durations and offsets** are numbers of seconds. Fields that may be fractional say so.
- **Unknown JSON fields** are ignored by both sides, so fields can be added without breaking older binaries.
- **Authentication:** every `/v1` route requires the shared secret. A request without a valid one gets `401` with code `unauthenticated`. How the secret is presented is specified with the authentication implementation.

| Operation | Route | Purpose |
|---|---|---|
| Claim | `POST /v1/jobs/claim` | Get one job, or learn there is none |
| Heartbeat | `POST /v1/jobs/{id}/heartbeat` | Keep the lease on a job |
| Complete | `POST /v1/jobs/{id}/complete` | Upload the finished clip |
| Fail | `POST /v1/jobs/{id}/fail` | Report that the job could not be done |

## The job

A job is one time range of one VOD to turn into a clip.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Job identifier. Opaque to the clipper, which sends it back unchanged in the path of the other operations |
| `attempt` | integer | How many times the job has been handed out, starting at 1 |
| `platform` | string | Source of the VOD. Selects the clipper's downloader. Only `twitch` today |
| `vod_id` | string | The platform's own identifier of the VOD |
| `start_seconds` | number, may be fractional | Start of the range, as an offset from the start of the VOD |
| `end_seconds` | number, may be fractional | End of the range. Greater than `start_seconds` |
| `streamer.login` | string | The platform's stable account name, as used in URLs |
| `streamer.display_name` | string | The name shown to viewers |
| `lease_expires_at` | time | When pogfarm takes the job back unless a heartbeat extends the lease |
| `heartbeat_interval_seconds` | integer | How often to send a heartbeat while holding the job |

Nothing in the job is specific to one platform except the value of `platform`.

The job says nothing about how the picture is arranged. The clipper decides that itself for every clip: it puts the streamer's webcam above the gameplay when it can find the webcam, and otherwise shows the whole frame over a blurred copy of itself.

```json
{
  "id": "42",
  "attempt": 1,
  "platform": "twitch",
  "vod_id": "2301234567",
  "start_seconds": 1055.5,
  "end_seconds": 1100,
  "streamer": { "login": "somestreamer", "display_name": "SomeStreamer" },
  "lease_expires_at": "2026-11-17T14:32:00Z",
  "heartbeat_interval_seconds": 30
}
```

## Leases and heartbeats

A claimed job is leased to the clipper that claimed it, identified by the `clipper` name it sent. pogfarm cannot see whether a clipper is still working, so the lease is how a job held by a dead or sleeping machine comes back.

1. Claim returns the job with `lease_expires_at` and `heartbeat_interval_seconds`. pogfarm chooses both; the interval is always well below the lease duration, so one missed heartbeat does not lose the job.
2. While it holds the job, including during the final upload, the clipper sends a heartbeat every `heartbeat_interval_seconds`. Each successful heartbeat moves `lease_expires_at` forward.
3. If the lease expires, pogfarm takes the job back. It is queued again with `attempt` increased by one, or marked failed once the attempt limit is reached.
4. From then on the former holder's heartbeats are answered with `lease_lost`.

A clipper whose heartbeat is answered with `lease_lost`, `already_completed` or `unknown_job` **abandons the job**: it stops working on it, removes its working files and does not call fail. The one exception is a clipper that has already finished rendering: it may still attempt the upload once, because of first-result-wins below.

A heartbeat that fails for another reason (network error, `internal`) says nothing about the lease. The clipper keeps working and keeps trying.

## First result wins

A job may run twice: a clipper finishes just as its lease expires and another clipper has already been given the same job. pogfarm therefore accepts results by this rule:

- The **first valid result** uploaded for a job becomes its result, **whoever sends it**. Holding the lease is not required for complete. This includes a result from a clipper that lost the lease, and a result for a job that has since been marked failed.
- Every later upload for that job is discarded and answered with `200` and `"result": "already_completed"`. The clipper treats that exactly like `accepted`: the job is done and it moves on.

Because of this, complete is safe to retry after a network error.

## Operations

### Claim

`POST /v1/jobs/claim`

Request:

```json
{ "clipper": "node-a" }
```

`clipper` is the caller's name. It must be non-empty and is the lease holder's identity in later calls.

Response `200`, a job was handed out:

```json
{ "job": { "id": "42", "...": "see The job" } }
```

Response `200`, nothing to do:

```json
{ "job": null, "retry_after_seconds": 10 }
```

`retry_after_seconds` is how long the clipper waits before asking again. pogfarm also answers this way when it has work but does not want this clipper to take more.

| Status | Code | When |
|---|---|---|
| 200 | – | A job, or `job: null` |
| 400 | `invalid_request` | Malformed body or empty `clipper` |
| 401 | `unauthenticated` | Missing or wrong secret |
| 500 | `internal` | pogfarm failed; retry later |

### Heartbeat

`POST /v1/jobs/{id}/heartbeat`

Request:

```json
{ "clipper": "node-a" }
```

Response `200`, the lease was extended:

```json
{ "lease_expires_at": "2026-11-17T14:34:00Z" }
```

| Status | Code | When | Clipper does |
|---|---|---|---|
| 200 | – | The caller holds the job; lease extended | Carries on |
| 400 | `invalid_request` | Malformed body or empty `clipper` | Fix the request |
| 401 | `unauthenticated` | Missing or wrong secret | – |
| 404 | `unknown_job` | No job with this ID | Abandons the job |
| 409 | `lease_lost` | The job is queued again, held by another clipper, or failed | Abandons the job |
| 409 | `already_completed` | The job already has a result | Abandons the job |
| 500 | `internal` | pogfarm failed | Carries on and retries |

### Complete

`POST /v1/jobs/{id}/complete`

A `multipart/form-data` request with exactly these parts, **in this order**, so pogfarm can check the small parts before reading the clip:

| Part name | Content type | Content |
|---|---|---|
| `metadata` | `application/json` | See below |
| `transcript` | `application/json` | See below |
| `clip` | `video/mp4` | The finished clip file |

`metadata`:

| Field | Type | Meaning |
|---|---|---|
| `clipper` | string | The caller's name |
| `sha256` | string | Lowercase hex SHA-256 of the bytes of the `clip` part |
| `size_bytes` | integer | Size of the `clip` part |
| `duration_seconds` | number, may be fractional | Length of the rendered clip |
| `width`, `height` | integer | Resolution in pixels |
| `tool_versions` | object of strings, optional | Tools that produced the clip, for example `{"ffmpeg": "7.1"}`. Informational |

`transcript`:

| Field | Type | Meaning |
|---|---|---|
| `language` | string | Spoken language as a BCP 47 tag such as `en`. Empty when unknown |
| `words` | array | Spoken words in order. Empty for a clip without speech |
| `words[].text` | string | The word |
| `words[].start_seconds`, `words[].end_seconds` | number, may be fractional | When the word is said, as offsets from the start of the **clip** |

```json
{
  "language": "en",
  "words": [
    { "text": "no", "start_seconds": 0.32, "end_seconds": 0.5 },
    { "text": "way", "start_seconds": 0.5, "end_seconds": 0.91 }
  ]
}
```

Response `200`:

```json
{ "result": "accepted" }
```

or, when the job already had a result:

```json
{ "result": "already_completed" }
```

Both mean the job is done. pogfarm verifies the checksum before accepting; a result is only ever stored whole.

| Status | Code | When | Clipper does |
|---|---|---|---|
| 200 | – | `accepted` or `already_completed` | Job is done |
| 400 | `invalid_request` | Missing part, wrong order, malformed JSON, invalid field | Reports fail, not retryable |
| 401 | `unauthenticated` | Missing or wrong secret | – |
| 404 | `unknown_job` | No job with this ID | Drops the result |
| 413 | `payload_too_large` | A part exceeds pogfarm's configured limit | Reports fail, not retryable |
| 422 | `checksum_mismatch` | The clip received does not match `sha256` | Retries the upload |
| 500 | `internal` | pogfarm failed | Retries the upload |

There is no `lease_lost` here: complete does not depend on the lease.

### Fail

`POST /v1/jobs/{id}/fail`

Request:

```json
{ "clipper": "node-a", "reason": "VOD was deleted", "retryable": false }
```

- `reason` is a short explanation for people, shown in status output.
- `retryable` says whether another attempt could succeed. A deleted VOD is not retryable; a network error, a full disk or a shutdown is.

pogfarm queues the job again if `retryable` is true and attempts remain, and marks it failed otherwise.

Response `204`, no body: the report was recorded.

| Status | Code | When | Clipper does |
|---|---|---|---|
| 204 | – | Recorded | Moves on |
| 400 | `invalid_request` | Malformed body or empty `clipper` | Moves on |
| 401 | `unauthenticated` | Missing or wrong secret | – |
| 404 | `unknown_job` | No job with this ID | Moves on |
| 409 | `lease_lost` | The caller does not hold the job; the report is ignored | Moves on |
| 409 | `already_completed` | The job already has a result; the report is ignored | Moves on |
| 500 | `internal` | pogfarm failed | Retries, then moves on; the lease will expire |

## Errors

Every response with a 4xx or 5xx status has this body:

```json
{ "error": { "code": "lease_lost", "message": "job 42 is held by another clipper" } }
```

Clients act on `code` and never on `message`, which is for logs and never contains secrets.

| Code | Status | Meaning |
|---|---|---|
| `unauthenticated` | 401 | The shared secret is missing or wrong |
| `invalid_request` | 400 | The request is malformed or a field is invalid |
| `unknown_job` | 404 | No job has this ID |
| `lease_lost` | 409 | The caller no longer holds the job |
| `already_completed` | 409 | The job already has a result (heartbeat and fail only) |
| `payload_too_large` | 413 | A part of the upload exceeds pogfarm's limit |
| `checksum_mismatch` | 422 | The clip does not match the checksum sent with it |
| `internal` | 500 | pogfarm failed; the call can be retried |

A client that receives a code it does not know treats it by status: 5xx as retryable, 4xx as not.
