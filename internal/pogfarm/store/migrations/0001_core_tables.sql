-- Core tables: streamers -> vods -> candidates -> jobs -> clips -> uploads.
--
-- Conventions, which later migrations keep:
--
--   * Times are TEXT in UTC, always in the fixed-width form
--     YYYY-MM-DDTHH:MM:SS.mmmZ (store.FormatTime). Because the width is fixed,
--     comparing two times as strings compares them as times, which the queue
--     relies on to find expired leases. Every time column has a CHECK on the
--     shape so a value in another format cannot get in. An optional time is
--     NULL when it is not set.
--   * Durations and offsets are REAL seconds, as in the job contract.
--   * Optional text (an error message, a clipper name) is '' when not set.
--   * platform holds the contract's platform name (api.Platform, 'twitch'
--     today). A table that hangs off a streamer, VOD or job carries the
--     platform itself, and its foreign key includes it, so a row can never
--     point at a parent on another platform.
--   * Status columns hold the names of the constants in the store package and
--     are limited to them by a CHECK.
--   * Tables are STRICT: a value of the wrong type is an error.

CREATE TABLE streamers (
    id               INTEGER PRIMARY KEY,
    platform         TEXT    NOT NULL CHECK (platform <> ''),
    -- The platform's own, permanent id of the account.
    platform_user_id TEXT    NOT NULL CHECK (platform_user_id <> ''),
    -- The account name used in URLs. It can change, so it is not the key.
    login            TEXT    NOT NULL CHECK (login <> ''),
    display_name     TEXT    NOT NULL,
    -- 1 while the streamer is on the allowlist and should be processed.
    enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at       TEXT    NOT NULL CHECK (created_at GLOB '????-??-??T??:??:??.???Z'),
    updated_at       TEXT    NOT NULL CHECK (updated_at GLOB '????-??-??T??:??:??.???Z'),
    UNIQUE (platform, platform_user_id),
    -- Target of the foreign key from vods.
    UNIQUE (id, platform)
) STRICT;

-- Allowlist sync looks a streamer up by the login in the config file.
CREATE INDEX streamers_platform_login ON streamers (platform, login);

CREATE TABLE vods (
    id               INTEGER PRIMARY KEY,
    platform         TEXT    NOT NULL CHECK (platform <> ''),
    platform_vod_id  TEXT    NOT NULL CHECK (platform_vod_id <> ''),
    streamer_id      INTEGER NOT NULL,
    title            TEXT    NOT NULL DEFAULT '',
    duration_seconds REAL    NOT NULL CHECK (duration_seconds >= 0),
    started_at       TEXT    NOT NULL CHECK (started_at GLOB '????-??-??T??:??:??.???Z'),
    ended_at         TEXT    NOT NULL CHECK (ended_at GLOB '????-??-??T??:??:??.???Z'),
    scan_status      TEXT    NOT NULL DEFAULT 'new'
                     CHECK (scan_status IN ('new', 'scanning', 'scanned', 'failed')),
    -- How many scans have been started, for the scanner's retry limit.
    scan_attempts    INTEGER NOT NULL DEFAULT 0 CHECK (scan_attempts >= 0),
    -- Why the last scan failed.
    scan_error       TEXT    NOT NULL DEFAULT '',
    -- Earliest time of the next scan attempt (retry backoff). NULL: any time.
    next_scan_at     TEXT    CHECK (next_scan_at GLOB '????-??-??T??:??:??.???Z'),
    created_at       TEXT    NOT NULL CHECK (created_at GLOB '????-??-??T??:??:??.???Z'),
    updated_at       TEXT    NOT NULL CHECK (updated_at GLOB '????-??-??T??:??:??.???Z'),
    UNIQUE (platform, platform_vod_id),
    -- Target of the foreign key from candidates.
    UNIQUE (id, platform),
    FOREIGN KEY (streamer_id, platform) REFERENCES streamers (id, platform),
    CHECK (ended_at >= started_at)
) STRICT;

-- The scanner picks VODs by scan status, oldest retry time first.
CREATE INDEX vods_scan ON vods (scan_status, next_scan_at);
-- A streamer's VODs, newest first.
CREATE INDEX vods_streamer ON vods (streamer_id, started_at);

CREATE TABLE candidates (
    id            INTEGER PRIMARY KEY,
    platform      TEXT    NOT NULL CHECK (platform <> ''),
    vod_id        INTEGER NOT NULL,
    -- The clip window, as offsets from the start of the VOD.
    start_seconds REAL    NOT NULL CHECK (start_seconds >= 0),
    end_seconds   REAL    NOT NULL,
    score         REAL    NOT NULL,
    signal        TEXT    NOT NULL CHECK (signal IN ('chat_spike', 'viewer_clip', 'both')),
    -- new: stored, nothing decided. queued: a render job exists for it.
    -- skipped: ranked below the per-VOD limit, never rendered.
    status        TEXT    NOT NULL DEFAULT 'new'
                  CHECK (status IN ('new', 'queued', 'skipped')),
    created_at    TEXT    NOT NULL CHECK (created_at GLOB '????-??-??T??:??:??.???Z'),
    updated_at    TEXT    NOT NULL CHECK (updated_at GLOB '????-??-??T??:??:??.???Z'),
    -- Building the candidates of a VOD twice cannot store a window twice.
    UNIQUE (vod_id, start_seconds, end_seconds),
    -- Target of the foreign key from jobs.
    UNIQUE (id, platform),
    FOREIGN KEY (vod_id, platform) REFERENCES vods (id, platform),
    CHECK (end_seconds > start_seconds)
) STRICT;

CREATE TABLE jobs (
    id                INTEGER PRIMARY KEY,
    platform          TEXT    NOT NULL CHECK (platform <> ''),
    -- A candidate has at most one render job.
    candidate_id      INTEGER NOT NULL UNIQUE,
    status            TEXT    NOT NULL DEFAULT 'queued'
                      CHECK (status IN ('queued', 'leased', 'done', 'failed')),
    -- How many times the job has been handed out.
    attempts          INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    -- The clipper that holds the job, or held it last.
    clipper           TEXT    NOT NULL DEFAULT '',
    -- Set exactly while the job is leased.
    lease_expires_at  TEXT    CHECK (lease_expires_at GLOB '????-??-??T??:??:??.???Z'),
    -- When the holder last claimed the job or sent a heartbeat for it.
    last_heartbeat_at TEXT    CHECK (last_heartbeat_at GLOB '????-??-??T??:??:??.???Z'),
    last_error        TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL CHECK (created_at GLOB '????-??-??T??:??:??.???Z'),
    updated_at        TEXT    NOT NULL CHECK (updated_at GLOB '????-??-??T??:??:??.???Z'),
    -- Target of the foreign key from clips.
    UNIQUE (id, platform),
    FOREIGN KEY (candidate_id, platform) REFERENCES candidates (id, platform),
    CHECK ((status = 'leased') = (lease_expires_at IS NOT NULL)),
    CHECK (status <> 'leased' OR clipper <> '')
) STRICT;

-- Claim: the oldest queued job.
CREATE INDEX jobs_status_created ON jobs (status, created_at, id);
-- Reaper: leased jobs whose lease has expired.
CREATE INDEX jobs_status_lease ON jobs (status, lease_expires_at);

CREATE TABLE clips (
    id                  INTEGER PRIMARY KEY,
    platform            TEXT    NOT NULL CHECK (platform <> ''),
    -- UNIQUE: a job has at most one accepted result. A second result for the
    -- same job cannot be stored.
    job_id              INTEGER NOT NULL UNIQUE,
    -- Path of the clip file, relative to the data directory.
    file_path           TEXT    NOT NULL,
    -- Lowercase hex SHA-256 of the file.
    sha256              TEXT    NOT NULL
                        CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    size_bytes          INTEGER NOT NULL CHECK (size_bytes >= 0),
    duration_seconds    REAL    NOT NULL CHECK (duration_seconds > 0),
    width               INTEGER NOT NULL CHECK (width > 0),
    height              INTEGER NOT NULL CHECK (height > 0),
    -- The contract's Transcript document.
    transcript_json     TEXT    NOT NULL CHECK (json_valid(transcript_json)),
    -- The clipper whose result was accepted.
    clipper             TEXT    NOT NULL DEFAULT '',
    review_status       TEXT    NOT NULL DEFAULT 'pending'
                        CHECK (review_status IN ('pending', 'approved', 'rejected')),
    -- When the clip was approved or rejected. NULL exactly while pending.
    reviewed_at         TEXT    CHECK (reviewed_at GLOB '????-??-??T??:??:??.???Z'),
    -- The review message. Both NULL until the clip has been sent.
    telegram_chat_id    INTEGER,
    telegram_message_id INTEGER,
    -- When cleanup deleted the file. NULL while the file exists.
    file_removed_at     TEXT    CHECK (file_removed_at GLOB '????-??-??T??:??:??.???Z'),
    created_at          TEXT    NOT NULL CHECK (created_at GLOB '????-??-??T??:??:??.???Z'),
    updated_at          TEXT    NOT NULL CHECK (updated_at GLOB '????-??-??T??:??:??.???Z'),
    -- One message reviews one clip. Rows that were not sent yet (NULLs) do
    -- not collide.
    UNIQUE (telegram_chat_id, telegram_message_id),
    FOREIGN KEY (job_id, platform) REFERENCES jobs (id, platform),
    CHECK ((review_status = 'pending') = (reviewed_at IS NULL)),
    CHECK ((telegram_chat_id IS NULL) = (telegram_message_id IS NULL))
) STRICT;

-- Review: pending clips not sent yet (telegram_message_id IS NULL), oldest
-- first; also every clip of one review status.
CREATE INDEX clips_review ON clips (review_status, telegram_message_id, created_at);
-- Cleanup: decided clips whose file still exists, by decision time.
CREATE INDEX clips_cleanup ON clips (review_status, file_removed_at, reviewed_at);

CREATE TABLE uploads (
    id               INTEGER PRIMARY KEY,
    clip_id          INTEGER NOT NULL REFERENCES clips (id),
    -- Where the clip is posted. This names an upload target, not the
    -- platform the VOD came from.
    target_platform  TEXT    NOT NULL CHECK (target_platform <> ''),
    status           TEXT    NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'uploading', 'done', 'failed')),
    -- The target's id of the post, once there is one.
    external_post_id TEXT    NOT NULL DEFAULT '',
    attempts         INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error       TEXT    NOT NULL DEFAULT '',
    -- Earliest time of the next attempt (retry backoff). NULL: any time.
    next_attempt_at  TEXT    CHECK (next_attempt_at GLOB '????-??-??T??:??:??.???Z'),
    -- When the post was made. Set exactly when status is done.
    posted_at        TEXT    CHECK (posted_at GLOB '????-??-??T??:??:??.???Z'),
    created_at       TEXT    NOT NULL CHECK (created_at GLOB '????-??-??T??:??:??.???Z'),
    updated_at       TEXT    NOT NULL CHECK (updated_at GLOB '????-??-??T??:??:??.???Z'),
    -- A clip is never posted twice to the same place.
    UNIQUE (clip_id, target_platform),
    CHECK ((status = 'done') = (posted_at IS NOT NULL))
) STRICT;

-- Uploader: pending uploads that are due.
CREATE INDEX uploads_status_due ON uploads (status, next_attempt_at);
-- Daily cap and spacing: the latest posts on one target.
CREATE INDEX uploads_target_posted ON uploads (target_platform, posted_at);
