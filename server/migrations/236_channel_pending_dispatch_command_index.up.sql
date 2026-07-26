CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_pending_dispatch_command_idx
    ON channel_pending_dispatch (installation_id, dispatch_message_id)
    WHERE dispatch_message_id IS NOT NULL;
