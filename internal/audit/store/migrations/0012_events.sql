-- What was done to DevPods, jobs and machines, through the console or by
-- it: one row each — who (actor) did what (action) to which object, how
-- (via: web, cli, dp, token:<name>, system) and why (reason), with what
-- changed (detail); origin is the DevPod a dp call came from. The objects
-- themselves live in their clusters; this is their history.

CREATE TABLE IF NOT EXISTS {db}.events (
    ts         DateTime64(3, 'UTC'),
    cluster    LowCardinality(String),
    kind       LowCardinality(String),
    object     String,
    object_uid String,
    owner      String,
    actor      String,
    via        LowCardinality(String),
    action     LowCardinality(String),
    node       String,
    reason     String,
    detail     String,
    origin     String,
    client_ip  String,
    request_id String
) ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (cluster, kind, object, ts)
;
