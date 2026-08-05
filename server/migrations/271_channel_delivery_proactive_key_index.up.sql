CREATE UNIQUE INDEX CONCURRENTLY channel_delivery_proactive_key_uidx ON channel_delivery (installation_id, request_key) WHERE kind = 'proactive_push' AND request_key IS NOT NULL;
