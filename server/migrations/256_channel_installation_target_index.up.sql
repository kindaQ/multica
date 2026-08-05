CREATE UNIQUE INDEX CONCURRENTLY channel_installation_target_uidx ON channel_installation (workspace_id, target_type, target_id, channel_type);
