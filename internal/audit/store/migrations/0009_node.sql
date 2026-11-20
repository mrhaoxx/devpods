-- The machine a session's DevPod was on when it opened, and a proxied
-- HTTP request's: what management grants of audit are judged on (design
-- 2026-11-20 §8). Rows of before have none.

ALTER TABLE {db}.sessions
    ADD COLUMN IF NOT EXISTS node LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.http_requests
    ADD COLUMN IF NOT EXISTS node LowCardinality(String) DEFAULT ''
;
