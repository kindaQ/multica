CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_account_binding_channel_user_idx
    ON channel_account_binding (installation_id, channel_user_id);
