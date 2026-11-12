-- Every record says the cluster of its DevPod ("" in a console of one
-- cluster), written by whoever writes it: the gateway for a session and
-- what it records, the console for telemetry and the alerts it raises.
-- A session also says the gateway it came in at. (This replaces
-- session_origin, which the migration's own step folds into sessions
-- and drops.) Readers and writers name their columns, so a column added
-- here is read and written by them all the same.

ALTER TABLE {db}.sessions
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT '',
    ADD COLUMN IF NOT EXISTS gateway LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.channels
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.commands
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.file_events
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.policy_events
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.raw_segments
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.alerts
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.ssh_conns
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.processes
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.container_info
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.container_snapshots
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.metrics_container_1s
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.metrics_gpu_1s
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.metrics_rdma_1s
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.metrics_process_1s
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.metrics_container_1m
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.metrics_gpu_1m
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

ALTER TABLE {db}.metrics_rdma_1m
    ADD COLUMN IF NOT EXISTS cluster LowCardinality(String) DEFAULT ''
;

-- the minute rollups take the cluster from the seconds they roll up
DROP VIEW IF EXISTS {db}.metrics_container_1m_mv
;
CREATE MATERIALIZED VIEW IF NOT EXISTS {db}.metrics_container_1m_mv TO {db}.metrics_container_1m AS
SELECT toStartOfMinute(ts) AS ts, devpod_uid, any(cluster) AS cluster,
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
DROP VIEW IF EXISTS {db}.metrics_gpu_1m_mv
;
CREATE MATERIALIZED VIEW IF NOT EXISTS {db}.metrics_gpu_1m_mv TO {db}.metrics_gpu_1m AS
SELECT toStartOfMinute(ts) AS ts, devpod_uid, gpu_uuid, any(cluster) AS cluster,
    avgState(util) AS util_avg, maxState(util) AS util_max, quantileState(0.95)(util) AS util_p95,
    avgState(mem_used) AS mem_avg, maxState(mem_used) AS mem_max,
    avgState(power) AS power_avg, maxState(temp) AS temp_max
FROM {db}.metrics_gpu_1s
GROUP BY ts, devpod_uid, gpu_uuid
;
DROP VIEW IF EXISTS {db}.metrics_rdma_1m_mv
;
CREATE MATERIALIZED VIEW IF NOT EXISTS {db}.metrics_rdma_1m_mv TO {db}.metrics_rdma_1m AS
SELECT toStartOfMinute(ts) AS ts, devpod_uid, ibdev, port, any(cluster) AS cluster,
    avgState(tx_bps) AS tx_avg, avgState(rx_bps) AS rx_avg, maxState(tx_bps) AS tx_max, maxState(rx_bps) AS rx_max
FROM {db}.metrics_rdma_1s
GROUP BY ts, devpod_uid, ibdev, port
;
