-- Events are written in batches, each with a deduplication token: a batch
-- sent again (its write went in, yet timed out) is kept once.
ALTER TABLE {db}.events MODIFY SETTING non_replicated_deduplication_window = 10000;
