-- name: GetChannelAccountBindingByChannelUser :one
SELECT * FROM channel_account_binding
WHERE installation_id = @installation_id
  AND channel_user_id = @channel_user_id;

-- name: GetChannelAccountBindingByMulticaUser :one
SELECT * FROM channel_account_binding
WHERE installation_id = @installation_id
  AND multica_user_id = @multica_user_id;

-- name: CreateChannelAccountBinding :one
INSERT INTO channel_account_binding (
    installation_id,
    channel_type,
    channel_user_id,
    multica_user_id,
    config
) VALUES (
    @installation_id,
    @channel_type,
    @channel_user_id,
    @multica_user_id,
    COALESCE(sqlc.narg('config'), '{}'::jsonb)
)
ON CONFLICT (installation_id, channel_user_id) DO UPDATE
SET config = channel_account_binding.config || EXCLUDED.config,
    updated_at = now()
WHERE channel_account_binding.multica_user_id = EXCLUDED.multica_user_id
RETURNING *;

-- name: DeleteChannelAccountBindingByMulticaUser :exec
DELETE FROM channel_account_binding
WHERE installation_id = @installation_id
  AND multica_user_id = @multica_user_id;

-- name: CreateChannelAccountBindingToken :one
INSERT INTO channel_account_binding_token (
    token_hash,
    installation_id,
    channel_type,
    channel_user_id,
    source_message_id,
    expires_at
) VALUES (
    @token_hash,
    @installation_id,
    @channel_type,
    @channel_user_id,
    sqlc.narg('source_message_id'),
    LEAST(@expires_at, now() + INTERVAL '15 minutes')
)
RETURNING *;

-- name: ConsumeChannelAccountBindingToken :one
UPDATE channel_account_binding_token
SET consumed_at = now()
WHERE token_hash = @token_hash
  AND consumed_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: DeleteExpiredChannelAccountBindingTokens :execrows
DELETE FROM channel_account_binding_token
WHERE expires_at < now() - INTERVAL '1 day';

-- name: GetWorkspaceChannelSetting :one
SELECT * FROM workspace_channel_setting
WHERE workspace_id = @workspace_id
  AND channel_type = @channel_type;

-- name: UpsertWorkspaceChannelSetting :one
INSERT INTO workspace_channel_setting (
    workspace_id,
    channel_type,
    default_agent_id,
    notification_recipient_user_id,
    notification_enabled,
    notification_events
) VALUES (
    @workspace_id,
    @channel_type,
    sqlc.narg('default_agent_id'),
    sqlc.narg('notification_recipient_user_id'),
    @notification_enabled,
    @notification_events
)
ON CONFLICT (workspace_id, channel_type) DO UPDATE
SET default_agent_id = EXCLUDED.default_agent_id,
    notification_recipient_user_id = EXCLUDED.notification_recipient_user_id,
    notification_enabled = EXCLUDED.notification_enabled,
    notification_events = EXCLUDED.notification_events,
    updated_at = now()
RETURNING *;

-- name: ClearWorkspaceChannelRecipientByUser :execrows
UPDATE workspace_channel_setting
SET notification_recipient_user_id = NULL,
    notification_enabled = FALSE,
    updated_at = now()
WHERE notification_recipient_user_id = @user_id;

-- name: ClearWorkspaceChannelRecipientForMember :execrows
UPDATE workspace_channel_setting
SET notification_recipient_user_id = NULL,
    notification_enabled = FALSE,
    updated_at = now()
WHERE workspace_id = @workspace_id
  AND notification_recipient_user_id = @user_id;

-- name: ClearWorkspaceChannelDefaultAgent :execrows
UPDATE workspace_channel_setting
SET default_agent_id = NULL,
    updated_at = now()
WHERE default_agent_id = @agent_id;

-- name: GetPublicChatSessionForChannel :one
SELECT public_chat_session_id
FROM channel_pending_dispatch
WHERE installation_id = @installation_id
  AND channel_chat_id = @channel_chat_id
  AND channel_thread_id IS NOT DISTINCT FROM sqlc.narg('channel_thread_id')
ORDER BY created_at ASC
LIMIT 1;

-- name: CreateChannelPendingDispatch :one
INSERT INTO channel_pending_dispatch (
    installation_id,
    channel_type,
    public_workspace_id,
    public_chat_session_id,
    channel_chat_id,
    channel_thread_id,
    channel_message_id,
    sender_channel_user_id,
    sender_multica_user_id,
    content,
    source_payload
) VALUES (
    @installation_id,
    @channel_type,
    @public_workspace_id,
    @public_chat_session_id,
    @channel_chat_id,
    sqlc.narg('channel_thread_id'),
    @channel_message_id,
    @sender_channel_user_id,
    sqlc.narg('sender_multica_user_id'),
    @content,
    @source_payload
)
RETURNING *;

-- name: GetPendingDispatchByChannelMessage :one
SELECT *
FROM channel_pending_dispatch
WHERE installation_id = @installation_id
  AND channel_message_id = @channel_message_id
FOR UPDATE;

-- name: MarkChannelPendingDispatchDispatched :one
UPDATE channel_pending_dispatch
SET status = 'dispatched',
    dispatch_message_id = @dispatch_message_id,
    dispatched_by_channel_user_id = @dispatched_by_channel_user_id,
    dispatched_by_multica_user_id = sqlc.narg('dispatched_by_multica_user_id'),
    target_channel_user_id = @target_channel_user_id,
    target_multica_user_id = @target_multica_user_id,
    target_workspace_id = @target_workspace_id,
    target_agent_id = @target_agent_id,
    target_chat_session_id = sqlc.narg('target_chat_session_id'),
    dispatched_at = now(),
    failure_code = NULL,
    updated_at = now()
WHERE id = @id
  AND status = 'pending'
RETURNING *;

-- name: ListChannelPendingDispatches :many
SELECT *
FROM channel_pending_dispatch
WHERE public_workspace_id = @public_workspace_id
  AND status = 'pending'
ORDER BY created_at ASC
LIMIT @page_size;

-- name: EnqueueChannelNotificationDelivery :execrows
INSERT INTO channel_notification_delivery (
    idempotency_key,
    event_type,
    workspace_id,
    issue_id,
    task_id,
    chat_session_id,
    installation_id,
    recipient_user_id,
    recipient_channel_user_id,
    payload
) VALUES (
    @idempotency_key,
    @event_type,
    @workspace_id,
    sqlc.narg('issue_id'),
    sqlc.narg('task_id'),
    sqlc.narg('chat_session_id'),
    @installation_id,
    @recipient_user_id,
    @recipient_channel_user_id,
    @payload
)
ON CONFLICT (idempotency_key) DO NOTHING;

-- name: ClaimChannelNotificationDelivery :one
WITH candidate AS (
    SELECT id
    FROM channel_notification_delivery
    WHERE (
        status = 'pending'
        AND next_attempt_at <= now()
    ) OR (
        status = 'sending'
        AND lease_expires_at < now()
    )
    ORDER BY next_attempt_at ASC, created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE channel_notification_delivery delivery
SET status = 'sending',
    attempt_count = delivery.attempt_count + 1,
    lease_token = gen_random_uuid(),
    lease_expires_at = now() + INTERVAL '1 minute',
    updated_at = now()
FROM candidate
WHERE delivery.id = candidate.id
RETURNING delivery.*;

-- name: MarkChannelNotificationDeliverySent :execrows
UPDATE channel_notification_delivery
SET status = 'sent',
    channel_message_id = @channel_message_id,
    sent_at = now(),
    lease_token = NULL,
    lease_expires_at = NULL,
    last_error_code = NULL,
    updated_at = now()
WHERE id = @id
  AND status = 'sending'
  AND lease_token = @lease_token;

-- name: RetryChannelNotificationDelivery :execrows
UPDATE channel_notification_delivery
SET status = CASE WHEN attempt_count >= @max_attempts THEN 'dead' ELSE 'pending' END,
    next_attempt_at = @next_attempt_at,
    lease_token = NULL,
    lease_expires_at = NULL,
    last_error_code = @last_error_code,
    updated_at = now()
WHERE id = @id
  AND status = 'sending'
  AND lease_token = @lease_token;

-- name: CancelChannelNotificationDelivery :execrows
UPDATE channel_notification_delivery
SET status = 'cancelled',
    lease_token = NULL,
    lease_expires_at = NULL,
    last_error_code = @last_error_code,
    updated_at = now()
WHERE id = @id
  AND status = 'sending'
  AND lease_token = @lease_token;
