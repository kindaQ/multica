CREATE INDEX CONCURRENTLY channel_delivery_task_idx ON channel_delivery (task_id, created_at) WHERE task_id IS NOT NULL;
