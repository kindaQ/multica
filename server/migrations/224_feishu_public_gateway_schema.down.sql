DROP TRIGGER IF EXISTS trg_enqueue_feishu_issue_notification ON issue;
DROP FUNCTION IF EXISTS enqueue_feishu_issue_notification();

DROP TABLE IF EXISTS channel_notification_delivery;
DROP TABLE IF EXISTS channel_pending_dispatch;
DROP TABLE IF EXISTS workspace_channel_setting;
DROP TABLE IF EXISTS channel_account_binding_token;
DROP TABLE IF EXISTS channel_account_binding;
DROP TABLE IF EXISTS instance_state;

ALTER TABLE "user"
    DROP COLUMN IF EXISTS default_workspace_id;
