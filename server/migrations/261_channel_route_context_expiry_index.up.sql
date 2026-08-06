CREATE INDEX CONCURRENTLY channel_route_context_expiry_idx ON channel_route_context (expires_at) WHERE consumed_at IS NULL;
