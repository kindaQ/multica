-- Durable state for squad-scoped channel routing. Relationships are enforced
-- and cleaned up by the application; channel tables intentionally have no
-- foreign keys or cascading actions.

ALTER TABLE channel_installation
    ADD COLUMN target_type TEXT NOT NULL DEFAULT 'agent'
        CHECK (target_type IN ('agent', 'squad')),
    ADD COLUMN target_id UUID;

UPDATE channel_installation SET target_id = agent_id;

ALTER TABLE channel_installation
    ALTER COLUMN target_id SET NOT NULL,
    ALTER COLUMN agent_id DROP NOT NULL,
    ADD CONSTRAINT channel_installation_target_shape_check CHECK (
        (target_type = 'agent' AND agent_id IS NOT NULL AND target_id = agent_id)
        OR (target_type = 'squad' AND agent_id IS NULL)
    );

ALTER TABLE channel_chat_session_binding
    ADD COLUMN agent_id UUID;

UPDATE channel_chat_session_binding b
SET agent_id = s.agent_id
FROM chat_session s
WHERE s.id = b.chat_session_id;

ALTER TABLE channel_chat_session_binding
    ALTER COLUMN agent_id SET NOT NULL;

CREATE TABLE channel_route_context (
    id                   UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id         UUID NOT NULL,
    installation_id      UUID NOT NULL,
    channel_type         TEXT NOT NULL,
    conversation_key     TEXT NOT NULL,
    channel_user_id      TEXT NOT NULL,
    issue_id             UUID,
    agent_id             UUID,
    source_message_id    TEXT,
    expires_at           TIMESTAMPTZ NOT NULL,
    consumed_at          TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (issue_id IS NOT NULL OR agent_id IS NOT NULL)
);

CREATE TABLE channel_delivery (
    id                           UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id                 UUID NOT NULL,
    installation_id              UUID NOT NULL,
    channel_type                 TEXT NOT NULL,
    kind                         TEXT NOT NULL CHECK (
        kind IN ('conversation_reply', 'proactive_push', 'event_notification')
    ),
    request_key                  TEXT,
    route_type                   TEXT NOT NULL CHECK (route_type IN ('chat', 'issue', 'none')),
    reply_policy                 TEXT NOT NULL DEFAULT 'disabled' CHECK (
        reply_policy IN ('disabled', 'chat_route', 'issue_route')
    ),
    task_id                      UUID,
    chat_session_id              UUID,
    issue_id                     UUID,
    agent_id                     UUID,
    source_user_id               UUID,
    destination_channel_user_id  TEXT,
    destination_chat_id          TEXT,
    destination_thread_id        TEXT,
    destination_message_id       TEXT,
    status                       TEXT NOT NULL DEFAULT 'pending' CHECK (
        status IN ('pending', 'sending', 'sent', 'failed', 'cancelled')
    ),
    lease_token                  UUID,
    lease_expires_at             TIMESTAMPTZ,
    attempt_count                INT NOT NULL DEFAULT 0,
    next_attempt_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    terminal_reason              TEXT,
    last_error                   TEXT,
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (destination_channel_user_id IS NOT NULL OR destination_chat_id IS NOT NULL)
);

CREATE TABLE channel_delivery_message (
    id                   UUID NOT NULL DEFAULT gen_random_uuid(),
    delivery_id          UUID NOT NULL,
    source_comment_id    UUID,
    ordinal              INT NOT NULL DEFAULT 0,
    idempotency_key      TEXT NOT NULL,
    channel_message_id   TEXT,
    status               TEXT NOT NULL DEFAULT 'pending' CHECK (
        status IN ('pending', 'sending', 'sent', 'failed')
    ),
    attempt_count        INT NOT NULL DEFAULT 0,
    last_error           TEXT,
    sent_at              TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
