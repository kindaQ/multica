CREATE INDEX CONCURRENTLY IF NOT EXISTS channel_notification_delivery_queue_idx
    ON channel_notification_delivery (status, next_attempt_at, created_at)
    WHERE status IN ('pending', 'sending');
