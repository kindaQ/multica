CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS workspace_channel_setting_idx
    ON workspace_channel_setting (workspace_id, channel_type);
