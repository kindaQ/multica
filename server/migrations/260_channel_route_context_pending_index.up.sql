CREATE UNIQUE INDEX CONCURRENTLY channel_route_context_pending_uidx ON channel_route_context (installation_id, conversation_key, channel_user_id) WHERE consumed_at IS NULL;
