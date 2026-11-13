-- HTTP requests a gateway's HTTP proxy served to DevPod ports (port
-- links), one row each — one per WebSocket connection, written when it
-- closes. Metadata only: no bodies. The query string is kept as sent.
-- Like every record, a row says its DevPod's cluster and the gateway that
-- served it, written by that gateway. request_id breaks ties of ts when a
-- search pages.

CREATE TABLE IF NOT EXISTS {db}.http_requests (
    ts            DateTime64(3, 'UTC'),
    duration_ms   UInt32,
    cluster       LowCardinality(String),
    gateway       LowCardinality(String),
    gateway_pod   String,
    devpod        String,
    devpod_uid    String,
    owner         String,
    port          UInt16,
    access        LowCardinality(String),
    user          String,
    method        LowCardinality(String),
    host          String,
    path          String,
    query         String,
    status        UInt16,
    bytes_in      UInt64,
    bytes_out     UInt64,
    websocket     Bool,
    client_ip     String,
    forwarded_for String,
    user_agent    String,
    request_id    String
) ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (devpod_uid, ts)
SETTINGS non_replicated_deduplication_window = 10000
;
