-- A session's last activity: an open session's row is written again every
-- minute (its bytes so far), and its rows expire by this, not by when it
-- opened — a session longer than the retention is not dropped while it
-- lasts (the TTL itself is set with the others, after the migrations).
-- Rows of before have their close (or opening) as last activity.

ALTER TABLE {db}.sessions
    ADD COLUMN IF NOT EXISTS last_active DateTime64(3, 'UTC') DEFAULT ifNull(closed_at, opened_at)
;
