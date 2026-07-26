CREATE INDEX CONCURRENTLY IF NOT EXISTS channel_account_binding_user_lookup_idx
    ON channel_account_binding (multica_user_id);
