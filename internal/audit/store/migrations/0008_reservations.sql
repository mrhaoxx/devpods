-- Node reservations, as the console kept and carried them out: one row
-- each time one is made, changed (its period, a cancel) or acted on (the
-- other users' DevPods hibernated at its start). The reservations
-- themselves are objects in the console's cluster, kept a while after
-- they end; this is their history.

CREATE TABLE IF NOT EXISTS {db}.reservations (
    ts           DateTime64(3, 'UTC'),
    cluster      LowCardinality(String),
    id           String,
    node         String,
    user         String,
    start        DateTime64(3, 'UTC'),
    end          DateTime64(3, 'UTC'),
    extends      String,
    cancelled_by String,
    event        LowCardinality(String),
    detail       String
) ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (cluster, node, start, id, ts)
;
