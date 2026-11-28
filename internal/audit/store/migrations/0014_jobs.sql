-- Queue jobs that have ended, kept whole once they leave the cluster: the
-- job as submitted (spec, JSON: what it is shown and submitted again
-- from) and how it went (status, JSON: each rank's end and output), with
-- what they are listed by beside. One row each, written once (a write
-- sent again with its token is kept once).

CREATE TABLE IF NOT EXISTS {db}.jobs (
    id           String,
    uid          String,
    cluster      LowCardinality(String),
    owner        String,
    title        String,
    phase        LowCardinality(String),
    submitted_at DateTime64(3, 'UTC'),
    finished_at  DateTime64(3, 'UTC'),
    spec         String CODEC(ZSTD(3)),
    status       String CODEC(ZSTD(3)),
    INDEX id_idx id TYPE bloom_filter GRANULARITY 4
) ENGINE = MergeTree
PARTITION BY toYYYYMM(finished_at)
ORDER BY (cluster, finished_at, id)
SETTINGS non_replicated_deduplication_window = 10000
;
