-- Where a session came in and which cluster its DevPod is in, for a
-- gateway that serves several clusters. A table of its own rather than
-- columns on sessions: gateways and consoles of an earlier version read
-- and write sessions by its full column list, and go on working.

CREATE TABLE IF NOT EXISTS {db}.session_origin (
    session_id  String,
    cluster     LowCardinality(String),
    gateway     LowCardinality(String),
    opened_at   DateTime64(3, 'UTC'),
    version     UInt64
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(opened_at)
ORDER BY (session_id)
SETTINGS non_replicated_deduplication_window = 10000
;
