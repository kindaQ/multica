CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS instance_state_singleton_idx
    ON instance_state (singleton_key);
