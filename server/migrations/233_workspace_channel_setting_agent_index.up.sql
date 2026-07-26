CREATE INDEX CONCURRENTLY IF NOT EXISTS workspace_channel_setting_agent_idx
    ON workspace_channel_setting (default_agent_id)
    WHERE default_agent_id IS NOT NULL;
