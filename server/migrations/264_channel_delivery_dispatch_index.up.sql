CREATE INDEX CONCURRENTLY channel_delivery_dispatch_idx ON channel_delivery (status, next_attempt_at) WHERE status IN ('pending', 'sending');
