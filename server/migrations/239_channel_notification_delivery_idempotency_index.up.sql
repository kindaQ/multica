CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS channel_notification_delivery_idempotency_idx
    ON channel_notification_delivery (idempotency_key);
