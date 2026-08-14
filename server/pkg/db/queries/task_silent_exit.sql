-- name: ListSilentExitMonitorWorkspaces :many
SELECT id, settings
FROM workspace
WHERE settings @> '{"task_silent_exit_monitor":{"enabled":true}}'::jsonb
ORDER BY id;

-- name: ListSilentExitCandidates :many
SELECT
    t.id AS task_id,
    w.id AS workspace_id,
    t.issue_id,
    t.agent_id,
    t.completed_at,
    i.status AS issue_status,
    i.title AS issue_title,
    i.number AS issue_number,
    w.issue_prefix,
    w.slug AS workspace_slug,
    a.name AS agent_name,
    COALESCE(last_output.id, t.trigger_comment_id) AS parent_comment_id,
    COALESCE(last_output.content, NULLIF(t.result->>'output', ''), '') AS last_progress
FROM agent_task_queue t
JOIN issue i ON i.id = t.issue_id
JOIN workspace w ON w.id = i.workspace_id
JOIN agent a ON a.id = t.agent_id
LEFT JOIN LATERAL (
    SELECT c.id, c.content
    FROM comment c
    WHERE c.source_task_id = t.id
      AND c.workspace_id = w.id
      AND c.issue_id = t.issue_id
      AND c.type = 'comment'
      AND c.content NOT LIKE '⚠️ 工作流可能已静默停止%'
      AND c.content NOT LIKE '⚠️ 工作流可能异常停止，请关注%'
    ORDER BY c.created_at DESC, c.id DESC
    LIMIT 1
) last_output ON TRUE
WHERE w.id = @workspace_id
  AND t.status = 'completed'
  AND t.issue_id IS NOT NULL
  AND t.completed_at >= @completed_after
  AND t.completed_at <= @completed_before
  AND (
      t.delegated_from_task_id IS NOT NULL
      OR EXISTS (
          SELECT 1
          FROM comment trigger_comment
          JOIN channel_delivery_message incoming_message
            ON incoming_message.source_comment_id = trigger_comment.parent_id
           AND incoming_message.status = 'sent'
          JOIN channel_delivery incoming_delivery
            ON incoming_delivery.id = incoming_message.delivery_id
           AND incoming_delivery.workspace_id = w.id
           AND incoming_delivery.issue_id = t.issue_id
           AND incoming_delivery.route_type = 'issue'
           AND incoming_delivery.reply_policy = 'issue_route'
           AND incoming_delivery.status = 'sent'
           AND (
               incoming_delivery.request_key ~ '(^|:)(approval_required|human_required)(:|$)'
               OR incoming_delivery.request_key LIKE 'task-watchdog:silent-exit:%'
           )
          WHERE trigger_comment.id = t.trigger_comment_id
            AND trigger_comment.workspace_id = w.id
            AND trigger_comment.issue_id = t.issue_id
            AND trigger_comment.author_type = 'member'
      )
  )
  AND NOT EXISTS (
      SELECT 1
      FROM agent_task_queue successor
      WHERE successor.delegated_from_task_id = t.id
  )
  AND NOT EXISTS (
      SELECT 1
      FROM comment outcome_comment
      JOIN channel_delivery_message outcome_message
        ON outcome_message.source_comment_id = outcome_comment.id
       AND outcome_message.status = 'sent'
      JOIN channel_delivery outcome_delivery
        ON outcome_delivery.id = outcome_message.delivery_id
       AND outcome_delivery.workspace_id = w.id
       AND outcome_delivery.issue_id = t.issue_id
       AND outcome_delivery.route_type = 'issue'
       AND outcome_delivery.status = 'sent'
      WHERE outcome_comment.source_task_id = t.id
        AND outcome_comment.workspace_id = w.id
        AND outcome_comment.issue_id = t.issue_id
        AND outcome_delivery.request_key ~ '(^|:)(approval_required|human_required|completed|failed)(:|$)'
  )
  AND NOT EXISTS (
      SELECT 1
      FROM channel_delivery alert_delivery
      WHERE alert_delivery.workspace_id = w.id
        AND alert_delivery.issue_id = t.issue_id
        AND alert_delivery.request_key = 'task-watchdog:silent-exit:' || t.id::text
  )
  AND NOT EXISTS (
      SELECT 1
      FROM comment alert_comment
      WHERE alert_comment.workspace_id = w.id
        AND alert_comment.issue_id = t.issue_id
        AND alert_comment.source_task_id = t.id
        AND (
            alert_comment.content LIKE '⚠️ 工作流可能已静默停止%'
            OR alert_comment.content LIKE '⚠️ 工作流可能异常停止，请关注%'
        )
  )
ORDER BY t.completed_at ASC
LIMIT @candidate_limit;
