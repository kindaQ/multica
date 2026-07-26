CREATE INDEX CONCURRENTLY IF NOT EXISTS workspace_channel_setting_recipient_idx
    ON workspace_channel_setting (notification_recipient_user_id)
    WHERE notification_recipient_user_id IS NOT NULL;
