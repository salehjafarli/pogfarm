-- The title the owner gave a clip by replying to its review message. It
-- replaces the generated title when the clip is posted; each upload target
-- still cuts it to its own length limit. NULL: no override, the generated
-- title is used. The length check only keeps a runaway value out
-- (store.MaxTitleOverrideLength).
ALTER TABLE clips ADD COLUMN title_override TEXT
    CHECK (title_override IS NULL OR length(title_override) BETWEEN 1 AND 1000);
