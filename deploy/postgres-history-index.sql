-- Run in the application's schema with psql autocommit enabled.
CREATE INDEX CONCURRENTLY IF NOT EXISTS samples_by_host_order
    ON samples (host_id, received_ms DESC, collected_at DESC, run_id DESC, sample_id DESC);
