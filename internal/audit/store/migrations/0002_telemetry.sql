-- Phase-2 container-side telemetry (spec §8.2). TTLs are applied by
-- Migrate from the Retention settings, as for 0001.

CREATE TABLE IF NOT EXISTS {db}.ssh_conns (
    devpod_uid  String,
    boot_id     String,
    pid         UInt32,
    starttime   UInt64,
    peer_addr   String,
    first_seen  DateTime64(3, 'UTC'),
    closed_at   Nullable(DateTime64(3, 'UTC')),
    session_id  String,
    user        LowCardinality(String),
    version     UInt64
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(first_seen)
ORDER BY (devpod_uid, first_seen, boot_id, pid, starttime)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.processes (
    devpod_uid   String,
    boot_id      String,
    pid          UInt32,
    starttime    UInt64,
    ppid         UInt32,
    exe          String,
    cmdline      String,
    uid          UInt32,
    first_seen   DateTime64(3, 'UTC'),
    exit_seen    Nullable(DateTime64(3, 'UTC')),
    source       LowCardinality(String),
    session_id   String,
    user         LowCardinality(String),
    peer         String,
    attribution  LowCardinality(String),
    version      UInt64
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(first_seen)
ORDER BY (devpod_uid, first_seen, boot_id, pid, starttime)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.container_info (
    ts          DateTime64(3, 'UTC'),
    devpod_uid  String,
    boot_id     String,
    info        String,
    version     UInt64
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(ts)
ORDER BY (devpod_uid, boot_id)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.container_snapshots (
    ts          DateTime64(3, 'UTC'),
    devpod_uid  String,
    data        String
) ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (devpod_uid, ts)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.metrics_container_1s (
    ts              DateTime64(3, 'UTC'),
    devpod_uid      String,
    boot_id         String,
    cpu             Float32,
    cpu_limit       Float32,
    throttled_usec  UInt64,
    mem_ws          UInt64,
    mem_anon        UInt64,
    mem_file        UInt64,
    mem_shmem       UInt64,
    mem_limit       UInt64,
    oom_kills       UInt64,
    io_r            Float32,
    io_w            Float32,
    io_r_iops       Float32,
    io_w_iops       Float32,
    net_rx          Float32,
    net_tx          Float32,
    net_tcp_rx      Float32,
    net_tcp_tx      Float32,
    net_other_rx    Float32,
    net_other_tx    Float32,
    net_rx_pkts     Float32,
    net_tx_pkts     Float32,
    net_errs        Float32,
    net_drops       Float32,
    net_kind        LowCardinality(String),
    pids            UInt32
) ENGINE = ReplacingMergeTree
PARTITION BY toDate(ts)
ORDER BY (devpod_uid, ts)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.metrics_gpu_1s (
    ts          DateTime64(3, 'UTC'),
    devpod_uid  String,
    gpu_uuid    String,
    idx         UInt8,
    name        LowCardinality(String),
    util        Float32,
    mem_used    Float32,
    mem_total   Float32,
    power       Float32,
    temp        Float32,
    sm_clock    Float32,
    proc_count  UInt16,
    shared      UInt8
) ENGINE = ReplacingMergeTree
PARTITION BY toDate(ts)
ORDER BY (devpod_uid, gpu_uuid, ts)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.metrics_rdma_1s (
    ts          DateTime64(3, 'UTC'),
    devpod_uid  String,
    ibdev       LowCardinality(String),
    port        UInt8,
    tx_bps      Float32,
    rx_bps      Float32,
    tx_pps      Float32,
    rx_pps      Float32,
    health      Map(String, UInt64),
    scope       LowCardinality(String)
) ENGINE = ReplacingMergeTree
PARTITION BY toDate(ts)
ORDER BY (devpod_uid, ibdev, port, ts)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.metrics_process_1s (
    ts          DateTime64(3, 'UTC'),
    devpod_uid  String,
    boot_id     String,
    pid         UInt32,
    starttime   UInt64,
    cpu         Float32,
    rss         UInt64,
    pss         UInt64,
    io_r        Float32,
    io_w        Float32,
    tcp_rx      Float32,
    tcp_tx      Float32,
    gpu         Array(UInt8),
    rdma        UInt8,
    session_id  String,
    peer        String
) ENGINE = ReplacingMergeTree
PARTITION BY toDate(ts)
ORDER BY (devpod_uid, ts, pid, starttime)
SETTINGS non_replicated_deduplication_window = 10000
;
CREATE TABLE IF NOT EXISTS {db}.metrics_container_1m (
    ts            DateTime('UTC'),
    devpod_uid    String,
    cpu_avg       AggregateFunction(avg, Float32),
    cpu_max       AggregateFunction(max, Float32),
    cpu_p95       AggregateFunction(quantile(0.95), Float32),
    cpu_limit     AggregateFunction(max, Float32),
    throttled_max AggregateFunction(max, UInt64),
    throttled_min AggregateFunction(min, UInt64),
    mem_ws_avg    AggregateFunction(avg, UInt64),
    mem_ws_max    AggregateFunction(max, UInt64),
    mem_anon_avg  AggregateFunction(avg, UInt64),
    mem_file_avg  AggregateFunction(avg, UInt64),
    mem_limit     AggregateFunction(max, UInt64),
    io_r_avg      AggregateFunction(avg, Float32),
    io_w_avg      AggregateFunction(avg, Float32),
    net_rx_avg    AggregateFunction(avg, Float32),
    net_tx_avg    AggregateFunction(avg, Float32),
    net_rx_max    AggregateFunction(max, Float32),
    net_tx_max    AggregateFunction(max, Float32),
    net_tcp_rx_avg AggregateFunction(avg, Float32),
    net_tcp_tx_avg AggregateFunction(avg, Float32)
) ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (devpod_uid, ts)
;
CREATE MATERIALIZED VIEW IF NOT EXISTS {db}.metrics_container_1m_mv TO {db}.metrics_container_1m AS
SELECT toStartOfMinute(ts) AS ts, devpod_uid,
    avgState(cpu) AS cpu_avg, maxState(cpu) AS cpu_max, quantileState(0.95)(cpu) AS cpu_p95,
    maxState(cpu_limit) AS cpu_limit, maxState(throttled_usec) AS throttled_max, minState(throttled_usec) AS throttled_min,
    avgState(mem_ws) AS mem_ws_avg, maxState(mem_ws) AS mem_ws_max,
    avgState(mem_anon) AS mem_anon_avg, avgState(mem_file) AS mem_file_avg, maxState(mem_limit) AS mem_limit,
    avgState(io_r) AS io_r_avg, avgState(io_w) AS io_w_avg,
    avgState(net_rx) AS net_rx_avg, avgState(net_tx) AS net_tx_avg,
    maxState(net_rx) AS net_rx_max, maxState(net_tx) AS net_tx_max,
    avgState(net_tcp_rx) AS net_tcp_rx_avg, avgState(net_tcp_tx) AS net_tcp_tx_avg
FROM {db}.metrics_container_1s
GROUP BY ts, devpod_uid
;
CREATE TABLE IF NOT EXISTS {db}.metrics_gpu_1m (
    ts          DateTime('UTC'),
    devpod_uid  String,
    gpu_uuid    String,
    util_avg    AggregateFunction(avg, Float32),
    util_max    AggregateFunction(max, Float32),
    util_p95    AggregateFunction(quantile(0.95), Float32),
    mem_avg     AggregateFunction(avg, Float32),
    mem_max     AggregateFunction(max, Float32),
    power_avg   AggregateFunction(avg, Float32),
    temp_max    AggregateFunction(max, Float32)
) ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (devpod_uid, gpu_uuid, ts)
;
CREATE MATERIALIZED VIEW IF NOT EXISTS {db}.metrics_gpu_1m_mv TO {db}.metrics_gpu_1m AS
SELECT toStartOfMinute(ts) AS ts, devpod_uid, gpu_uuid,
    avgState(util) AS util_avg, maxState(util) AS util_max, quantileState(0.95)(util) AS util_p95,
    avgState(mem_used) AS mem_avg, maxState(mem_used) AS mem_max,
    avgState(power) AS power_avg, maxState(temp) AS temp_max
FROM {db}.metrics_gpu_1s
GROUP BY ts, devpod_uid, gpu_uuid
;
CREATE TABLE IF NOT EXISTS {db}.metrics_rdma_1m (
    ts          DateTime('UTC'),
    devpod_uid  String,
    ibdev       LowCardinality(String),
    port        UInt8,
    tx_avg      AggregateFunction(avg, Float32),
    rx_avg      AggregateFunction(avg, Float32),
    tx_max      AggregateFunction(max, Float32),
    rx_max      AggregateFunction(max, Float32)
) ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (devpod_uid, ibdev, port, ts)
;
CREATE MATERIALIZED VIEW IF NOT EXISTS {db}.metrics_rdma_1m_mv TO {db}.metrics_rdma_1m AS
SELECT toStartOfMinute(ts) AS ts, devpod_uid, ibdev, port,
    avgState(tx_bps) AS tx_avg, avgState(rx_bps) AS rx_avg, maxState(tx_bps) AS tx_max, maxState(rx_bps) AS rx_max
FROM {db}.metrics_rdma_1s
GROUP BY ts, devpod_uid, ibdev, port
;
