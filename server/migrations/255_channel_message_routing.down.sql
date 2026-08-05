DROP TABLE channel_delivery_message;
DROP TABLE channel_delivery;
DROP TABLE channel_route_context;

ALTER TABLE channel_chat_session_binding
    DROP COLUMN agent_id;

-- Squad installations cannot be represented by the previous schema.
DELETE FROM channel_installation WHERE target_type = 'squad';

ALTER TABLE channel_installation
    DROP CONSTRAINT channel_installation_target_shape_check,
    ALTER COLUMN agent_id SET NOT NULL,
    DROP COLUMN target_id,
    DROP COLUMN target_type;
