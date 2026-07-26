CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_account_binding_multica_user_idx
    ON channel_account_binding (installation_id, multica_user_id);
