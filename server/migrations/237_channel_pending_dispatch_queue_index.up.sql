CREATE INDEX CONCURRENTLY IF NOT EXISTS channel_pending_dispatch_queue_idx
    ON channel_pending_dispatch (public_workspace_id, status, created_at DESC);
