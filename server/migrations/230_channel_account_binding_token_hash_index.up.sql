CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_account_binding_token_hash_idx
    ON channel_account_binding_token (token_hash);
