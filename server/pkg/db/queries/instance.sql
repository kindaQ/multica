-- name: ClaimInstanceSuperAdmin :one
INSERT INTO instance_state (singleton_key, super_admin_user_id)
VALUES (1, @user_id)
ON CONFLICT (singleton_key) DO UPDATE
SET singleton_key = instance_state.singleton_key
RETURNING *;

-- name: GetInstanceState :one
SELECT * FROM instance_state
WHERE singleton_key = 1;

-- name: LockInstanceState :one
SELECT * FROM instance_state
WHERE singleton_key = 1
FOR UPDATE;

-- name: IsInstanceSuperAdmin :one
SELECT EXISTS (
    SELECT 1
    FROM instance_state
    WHERE singleton_key = 1
      AND super_admin_user_id = @user_id
) AS is_super_admin;

-- name: CreatePublicGatewayRuntime :one
INSERT INTO agent_runtime (
    workspace_id,
    daemon_id,
    name,
    runtime_mode,
    provider,
    status,
    device_info,
    metadata,
    owner_id
) VALUES (
    @workspace_id,
    NULL,
    'Feishu Public Gateway',
    'local',
    'feishu_gateway',
    'offline',
    'System-owned transport runtime',
    '{"system":true,"purpose":"feishu_public_gateway"}'::jsonb,
    @owner_id
)
RETURNING *;

-- name: CreatePublicGatewayAgent :one
INSERT INTO agent (
    workspace_id,
    name,
    description,
    instructions,
    runtime_mode,
    runtime_config,
    runtime_id,
    visibility,
    permission_mode,
    status,
    max_concurrent_tasks,
    owner_id,
    custom_env,
    custom_args,
    kind,
    system_key
) VALUES (
    @workspace_id,
    'Multica Public Gateway',
    'System-owned Feishu public gateway. It stores unbound group messages and never runs them automatically.',
    '',
    'local',
    '{}'::jsonb,
    @runtime_id,
    'private',
    'private',
    'offline',
    1,
    @owner_id,
    '{}'::jsonb,
    '[]'::jsonb,
    'user',
    'feishu_public_gateway'
)
RETURNING *;

-- name: CompleteInstanceBootstrap :one
UPDATE instance_state
SET public_workspace_id = @public_workspace_id,
    public_agent_id = @public_agent_id,
    status = 'setup',
    initialized_at = COALESCE(initialized_at, now()),
    updated_at = now()
WHERE singleton_key = 1
  AND super_admin_user_id = @super_admin_user_id
RETURNING *;

-- name: SetInstancePublicChannelInstallation :one
UPDATE instance_state
SET public_channel_installation_id = @installation_id,
    status = 'ready',
    updated_at = now()
WHERE singleton_key = 1
  AND super_admin_user_id = @super_admin_user_id
  AND public_workspace_id IS NOT NULL
  AND public_agent_id IS NOT NULL
RETURNING *;

-- name: SetInstanceGatewayStatus :one
UPDATE instance_state
SET status = @status,
    updated_at = now()
WHERE singleton_key = 1
  AND super_admin_user_id = @super_admin_user_id
RETURNING *;

-- name: ReconcileInstancePublicChannelInstallation :execrows
UPDATE instance_state state
SET public_channel_installation_id = installation.id,
    status = 'ready',
    updated_at = now()
FROM channel_installation installation
WHERE state.singleton_key = 1
  AND state.public_workspace_id = installation.workspace_id
  AND state.public_agent_id = installation.agent_id
  AND installation.channel_type = 'feishu'
  AND installation.status = 'active'
  AND (
      state.public_channel_installation_id IS DISTINCT FROM installation.id
      OR state.status <> 'ready'
  );
