-- Phase-1 audit tables. {db} is replaced with the configured database.
-- Statements are separated by a line holding only ";". TTLs are not set
-- here: Migrate applies them on every run from --retention-days.

CREATE TABLE IF NOT EXISTS {db}.sessions (
    session_id         String,
    user               LowCardinality(String),
    devpod             String,
    devpod_uid         String,
    owner              LowCardinality(String),
    gateway_pod        LowCardinality(String),
    client_ip          String,
    pubkey_fp          String,
    auth_kind          LowCardinality(String),
    proxy_alias        String,
    backend_local_addr String,
    opened_at          DateTime64(3, 'UTC'),
    closed_at          Nullable(DateTime64(3, 'UTC')),
    close_reason       LowCardinality(String),
    bytes_in           Int64,
    bytes_out          Int64,
    policy_snapshot    String,
    gaps               UInt32,
    version            UInt64
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(opened_at)
ORDER BY (user, opened_at, session_id)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.channels (
    session_id   String,
    channel_id   UInt32,
    type         LowCardinality(String),
    command      String,
    subsystem    LowCardinality(String),
    target       String,
    pty          Bool,
    term         LowCardinality(String),
    opened_at    DateTime64(3, 'UTC'),
    closed_at    Nullable(DateTime64(3, 'UTC')),
    exit_status  Nullable(Int32),
    bytes_in     Int64,
    bytes_out    Int64,
    raw_recorded Bool,
    version      UInt64
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(opened_at)
ORDER BY (session_id, channel_id)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.commands (
    event_id    String,
    ts          DateTime64(3, 'UTC'),
    session_id  String,
    devpod_uid  String,
    user        LowCardinality(String),
    source      LowCardinality(String),
    channel_id  UInt32,
    pid         Int32,
    cwd         String,
    cmdline     String,
    exit_code   Nullable(Int32),
    duration_ms Int64,
    attribution LowCardinality(String),
    version     UInt64,
    INDEX cmdline_ngram cmdline TYPE ngrambf_v1(3, 65536, 2, 0) GRANULARITY 1
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(ts)
ORDER BY (devpod_uid, ts, event_id)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.file_events (
    event_id   String,
    ts         DateTime64(3, 'UTC'),
    session_id String,
    channel_id UInt32,
    user       LowCardinality(String),
    devpod_uid String,
    protocol   LowCardinality(String),
    op         LowCardinality(String),
    path       String,
    path2      String,
    size       Int64,
    sha256     String,
    complete   Bool,
    blocked    Bool,
    s3_key     String,
    INDEX path_bf path TYPE bloom_filter GRANULARITY 1,
    INDEX sha_bf sha256 TYPE bloom_filter GRANULARITY 1
) ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (devpod_uid, ts)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.policy_events (
    ts         DateTime64(3, 'UTC'),
    session_id String,
    user       LowCardinality(String),
    devpod_uid String,
    rule       LowCardinality(String),
    action     LowCardinality(String),
    detail     String
) ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY ts
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.raw_segments (
    session_id       String,
    seq              UInt32,
    s3_key           String,
    first_ts         DateTime64(3, 'UTC'),
    last_ts          DateTime64(3, 'UTC'),
    bytes            Int64,
    compressed_bytes Int64
) ENGINE = MergeTree
PARTITION BY toYYYYMM(first_ts)
ORDER BY (session_id, seq)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.alerts (
    ts         DateTime64(3, 'UTC'),
    devpod_uid String,
    session_id String,
    kind       LowCardinality(String),
    source     LowCardinality(String),
    detail     String
) ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY ts
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.access_log (
    ts             DateTime64(3, 'UTC'),
    viewer         LowCardinality(String),
    action         LowCardinality(String),
    target_session String,
    target_file    String,
    client_ip      String
) ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY ts
SETTINGS non_replicated_deduplication_window = 10000
