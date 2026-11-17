-- Uploads that need the owner to look by hand.
--
-- A row is left in 'uploading' when pogfarm stopped (or an upload ran out of
-- time) after the target was called and before the result was stored. The
-- post may or may not exist. When the uploader cannot ask the target, it
-- sets attention_at and attention_reason and leaves the status at
-- 'uploading', which is the truth: unknown. While attention_at is set the
-- uploader never touches the row, the clip's file is kept (cleanup only
-- removes files whose uploads are all 'done'), and the review cannot be
-- undone. The owner settles the row as done or pending, which clears both.

ALTER TABLE uploads ADD COLUMN attention_at TEXT
    CHECK (attention_at IS NULL
        OR (attention_at GLOB '????-??-??T??:??:??.???Z' AND status = 'uploading'));

-- Why the row needs a look. '' while attention_at is NULL.
ALTER TABLE uploads ADD COLUMN attention_reason TEXT NOT NULL DEFAULT '';
