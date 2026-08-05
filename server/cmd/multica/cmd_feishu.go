package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var feishuCmd = &cobra.Command{
	Use:   "feishu",
	Short: "Send messages through a workspace Feishu bot",
}

var feishuPushCmd = &cobra.Command{
	Use:   "push",
	Short: "Push a message to the Feishu bot owner",
	RunE:  runFeishuPush,
}

func init() {
	feishuPushCmd.Flags().String("installation-id", "", "Feishu installation ID (required)")
	feishuPushCmd.Flags().String("issue-id", "", "Optional issue ID used for quoted-reply routing")
	feishuPushCmd.Flags().String("content", "", "Message content (required)")
	feishuPushCmd.Flags().String("idempotency-key", "", "Stable retry key (required)")
	feishuPushCmd.Flags().String("reply-policy", "", "Reply policy: disabled, chat_route, or issue_route")
	feishuCmd.AddCommand(feishuPushCmd)
}

func runFeishuPush(cmd *cobra.Command, _ []string) error {
	workspaceID := strings.TrimSpace(resolveWorkspaceID(cmd))
	if workspaceID == "" {
		return errors.New("workspace ID not set: use --workspace-id or MULTICA_WORKSPACE_ID")
	}
	installationID, _ := cmd.Flags().GetString("installation-id")
	content, _ := cmd.Flags().GetString("content")
	key, _ := cmd.Flags().GetString("idempotency-key")
	if strings.TrimSpace(installationID) == "" || strings.TrimSpace(content) == "" || strings.TrimSpace(key) == "" {
		return errors.New("--installation-id, --content, and --idempotency-key are required")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	body := map[string]any{
		"installation_id": installationID,
		"content":         content,
		"idempotency_key": key,
	}
	if issueID, _ := cmd.Flags().GetString("issue-id"); strings.TrimSpace(issueID) != "" {
		body["issue_id"] = issueID
	}
	if policy, _ := cmd.Flags().GetString("reply-policy"); strings.TrimSpace(policy) != "" {
		body["reply_policy"] = policy
	}
	var result map[string]any
	path := "/api/workspaces/" + workspaceID + "/lark/deliveries"
	if err := client.PostJSON(cmd.Context(), path, body, &result); err != nil {
		return fmt.Errorf("push Feishu message: %w", err)
	}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
	return nil
}
