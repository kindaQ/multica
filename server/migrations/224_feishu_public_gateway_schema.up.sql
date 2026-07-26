-- Instance-level Feishu public gateway.
--
-- Relationships intentionally carry no foreign keys. Multica enforces them in
-- the application layer and performs dependent cleanup in the same transaction
-- as the owning resource mutation.
--
-- No indexes (including primary keys) are created in this migration. Every
-- unique and lookup index is built CONCURRENTLY in a following single-statement
-- migration, per the repository migration rules.

ALTER TABLE "user"
    ADD COLUMN default_workspace_id UUID;

-- Existing accounts keep their oldest membership as the initial routing
-- destination. New accounts set this atomically when their first workspace is
-- created or their first invitation is accepted.
UPDATE "user" account
SET default_workspace_id = (
    SELECT member.workspace_id
    FROM member
    WHERE member.user_id = account.id
    ORDER BY member.created_at ASC, member.id ASC
    LIMIT 1
)
WHERE EXISTS (
    SELECT 1
    FROM member
    WHERE member.user_id = account.id
);

CREATE TABLE instance_state (
    singleton_key                  SMALLINT NOT NULL DEFAULT 1
        CHECK (singleton_key = 1),
    super_admin_user_id            UUID NOT NULL,
    public_workspace_id            UUID,
    public_agent_id                UUID,
    public_channel_installation_id UUID,
    status                         TEXT NOT NULL DEFAULT 'uninitialized'
        CHECK (status IN ('uninitialized', 'setup', 'ready', 'paused')),
    initialized_at                 TIMESTAMPTZ,
    created_at                     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Existing self-hosted instances already have users when this migration lands.
-- Preserve "the first account is the super administrator" by selecting the
-- oldest account deterministically. Fresh databases have no users here; the
-- signup transaction claims the singleton when the first account is created.
INSERT INTO instance_state (super_admin_user_id)
SELECT id
FROM "user"
ORDER BY created_at ASC, id ASC
LIMIT 1;

CREATE TABLE channel_account_binding (
    id                UUID NOT NULL DEFAULT gen_random_uuid(),
    installation_id   UUID NOT NULL,
    channel_type       TEXT NOT NULL,
    channel_user_id    TEXT NOT NULL,
    multica_user_id    UUID NOT NULL,
    config             JSONB NOT NULL DEFAULT '{}'::jsonb,
    bound_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE channel_account_binding_token (
    token_hash       TEXT NOT NULL,
    installation_id UUID NOT NULL,
    channel_type     TEXT NOT NULL,
    channel_user_id  TEXT NOT NULL,
    source_message_id TEXT,
    expires_at       TIMESTAMPTZ NOT NULL,
    consumed_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT channel_account_binding_token_ttl_cap
        CHECK (expires_at <= created_at + INTERVAL '15 minutes')
);

CREATE TABLE workspace_channel_setting (
    workspace_id                  UUID NOT NULL,
    channel_type                  TEXT NOT NULL,
    default_agent_id              UUID,
    notification_recipient_user_id UUID,
    notification_enabled          BOOLEAN NOT NULL DEFAULT FALSE,
    notification_events           JSONB NOT NULL DEFAULT
        '["issue.done","issue.blocked","dispatch.completed","dispatch.failed"]'::jsonb,
    created_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE channel_pending_dispatch (
    id                              UUID NOT NULL DEFAULT gen_random_uuid(),
    installation_id                 UUID NOT NULL,
    channel_type                    TEXT NOT NULL,
    public_workspace_id             UUID NOT NULL,
    public_chat_session_id           UUID NOT NULL,
    channel_chat_id                  TEXT NOT NULL,
    channel_thread_id                TEXT,
    channel_message_id               TEXT NOT NULL,
    sender_channel_user_id           TEXT NOT NULL,
    sender_multica_user_id           UUID,
    content                          TEXT NOT NULL,
    source_payload                   JSONB NOT NULL DEFAULT '{}'::jsonb,
    status                           TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'dispatched', 'cancelled')),
    dispatch_message_id              TEXT,
    dispatched_by_channel_user_id    TEXT,
    dispatched_by_multica_user_id    UUID,
    target_channel_user_id           TEXT,
    target_multica_user_id           UUID,
    target_workspace_id              UUID,
    target_agent_id                  UUID,
    target_chat_session_id           UUID,
    dispatched_at                    TIMESTAMPTZ,
    failure_code                     TEXT,
    created_at                       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE channel_notification_delivery (
    id                        UUID NOT NULL DEFAULT gen_random_uuid(),
    idempotency_key           TEXT NOT NULL,
    event_type                TEXT NOT NULL,
    workspace_id              UUID NOT NULL,
    issue_id                  UUID,
    task_id                   UUID,
    chat_session_id           UUID,
    installation_id           UUID NOT NULL,
    recipient_user_id         UUID NOT NULL,
    recipient_channel_user_id TEXT NOT NULL,
    payload                   JSONB NOT NULL,
    render_version            INTEGER NOT NULL DEFAULT 1,
    status                    TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'sending', 'sent', 'dead', 'cancelled')),
    attempt_count             INTEGER NOT NULL DEFAULT 0
        CHECK (attempt_count >= 0),
    next_attempt_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_token               UUID,
    lease_expires_at          TIMESTAMPTZ,
    channel_message_id        TEXT,
    last_error_code           TEXT,
    sent_at                   TIMESTAMPTZ,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Capture terminal issue transitions inside the same transaction as the issue
-- update. This covers HTTP, batch, agent CLI and VCS-driven status mutations
-- without relying on an in-process event subscriber that could miss the
-- commit during a crash.
CREATE OR REPLACE FUNCTION enqueue_feishu_issue_notification()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    notification_event TEXT;
BEGIN
    IF NEW.status IS NOT DISTINCT FROM OLD.status
       OR NEW.status NOT IN ('done', 'blocked') THEN
        RETURN NEW;
    END IF;

    notification_event := 'issue.' || NEW.status;

    INSERT INTO channel_notification_delivery (
        idempotency_key,
        event_type,
        workspace_id,
        issue_id,
        installation_id,
        recipient_user_id,
        recipient_channel_user_id,
        payload
    )
    SELECT
        notification_event || ':' || NEW.id::text || ':' || NEW.updated_at::text,
        notification_event,
        NEW.workspace_id,
        NEW.id,
        state.public_channel_installation_id,
        setting.notification_recipient_user_id,
        binding.channel_user_id,
        jsonb_build_object(
            'status_changed', TRUE,
            'prev_status', OLD.status,
            'issue', jsonb_build_object(
                'id', NEW.id,
                'workspace_id', NEW.workspace_id,
                'number', NEW.number,
                'identifier', workspace.issue_prefix || '-' || NEW.number::text,
                'title', NEW.title,
                'status', NEW.status,
                'updated_at', NEW.updated_at
            )
        )
    FROM workspace_channel_setting setting
    JOIN instance_state state
      ON state.singleton_key = 1
     AND state.status = 'ready'
     AND state.public_channel_installation_id IS NOT NULL
    JOIN channel_account_binding binding
      ON binding.installation_id = state.public_channel_installation_id
     AND binding.multica_user_id = setting.notification_recipient_user_id
    JOIN workspace
      ON workspace.id = NEW.workspace_id
    WHERE setting.workspace_id = NEW.workspace_id
      AND setting.channel_type = 'feishu'
      AND setting.notification_enabled
      AND setting.notification_recipient_user_id IS NOT NULL
      AND setting.notification_events ? notification_event
    ON CONFLICT DO NOTHING;

    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_enqueue_feishu_issue_notification
AFTER UPDATE OF status ON issue
FOR EACH ROW
EXECUTE FUNCTION enqueue_feishu_issue_notification();
