CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_pending_dispatch_source_idx
    ON channel_pending_dispatch (installation_id, channel_message_id);
