CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_pending_dispatch_id_idx
    ON channel_pending_dispatch (id);
