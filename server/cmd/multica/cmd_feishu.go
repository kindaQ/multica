package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	feishuPushCmd.Flags().String("installation-id", "", "Feishu installation ID (optional when exactly one Bot is available to this agent)")
	feishuPushCmd.Flags().String("issue-id", "", "Optional issue ID used for quoted-reply routing")
	feishuPushCmd.Flags().String("content", "", "Message content (mutually exclusive with --post-file)")
	feishuPushCmd.Flags().String("post-file", "", "Path to a Feishu post JSON payload (mutually exclusive with --content)")
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
	postFile, _ := cmd.Flags().GetString("post-file")
	key, _ := cmd.Flags().GetString("idempotency-key")
	if strings.TrimSpace(key) == "" {
		return errors.New("--idempotency-key is required")
	}
	if (strings.TrimSpace(content) == "") == (strings.TrimSpace(postFile) == "") {
		return errors.New("exactly one of --content or --post-file is required")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	body := map[string]any{"idempotency_key": key}
	if strings.TrimSpace(postFile) != "" {
		post, err := readFeishuPostFile(postFile)
		if err != nil {
			return err
		}
		body["post"] = post
	} else {
		body["content"] = content
	}
	if strings.TrimSpace(installationID) != "" {
		body["installation_id"] = installationID
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

func readFeishuPostFile(path string) (json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --post-file: %w", err)
	}
	if len(data) == 0 || !json.Valid(data) {
		return nil, errors.New("--post-file must contain valid JSON")
	}

	var webhookPayload struct {
		MsgType string `json:"msg_type"`
		Content struct {
			Post json.RawMessage `json:"post"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &webhookPayload); err == nil && webhookPayload.MsgType != "" {
		if webhookPayload.MsgType != "post" || len(webhookPayload.Content.Post) == 0 {
			return nil, errors.New("--post-file webhook payload must use msg_type=post and contain content.post")
		}
		return webhookPayload.Content.Post, nil
	}

	var locales map[string]json.RawMessage
	if err := json.Unmarshal(data, &locales); err != nil || len(locales) == 0 {
		return nil, errors.New("--post-file must contain a Feishu post locale object")
	}
	return json.RawMessage(data), nil
}
