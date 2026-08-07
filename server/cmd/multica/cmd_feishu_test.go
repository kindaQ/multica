package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestFeishuPushOmitsInstallationForServerDiscovery(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspaces/workspace-1/lark/deliveries" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"installation_id":"install-1","delivery_id":"delivery-1","message_id":"message-1"}`))
	}))
	defer srv.Close()

	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "workspace-1")
	t.Setenv("MULTICA_TOKEN", "mat_task-token")
	t.Setenv("MULTICA_AGENT_ID", "agent-1")
	t.Setenv("MULTICA_TASK_ID", "task-1")

	cmd := newFeishuPushTestCommand()
	if err := cmd.Flags().Set("content", "Please review"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("idempotency-key", "task-1:review"); err != nil {
		t.Fatal(err)
	}
	if err := runFeishuPush(cmd, nil); err != nil {
		t.Fatalf("runFeishuPush() error = %v", err)
	}
	if _, exists := body["installation_id"]; exists {
		t.Fatalf("installation_id should be omitted for automatic discovery: %#v", body)
	}
}

func newFeishuPushTestCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.Flags().String("installation-id", "", "")
	cmd.Flags().String("issue-id", "", "")
	cmd.Flags().String("content", "", "")
	cmd.Flags().String("post-file", "", "")
	cmd.Flags().String("idempotency-key", "", "")
	cmd.Flags().String("reply-policy", "", "")
	return cmd
}

func TestFeishuPushAcceptsWebhookPostFile(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"installation_id":"install-1","delivery_id":"delivery-1","message_id":"message-1"}`))
	}))
	defer srv.Close()

	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "workspace-1")
	t.Setenv("MULTICA_TOKEN", "mat_task-token")
	t.Setenv("MULTICA_AGENT_ID", "agent-1")
	t.Setenv("MULTICA_TASK_ID", "task-1")

	path := filepath.Join(t.TempDir(), "notification.json")
	payload := `{"msg_type":"post","content":{"post":{"zh_cn":{"title":"阶段完成","content":[[{"tag":"text","text":"完成"}]]}}}}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newFeishuPushTestCommand()
	_ = cmd.Flags().Set("post-file", path)
	_ = cmd.Flags().Set("idempotency-key", "task-1:completed")
	if err := runFeishuPush(cmd, nil); err != nil {
		t.Fatalf("runFeishuPush() error = %v", err)
	}
	if _, exists := body["content"]; exists {
		t.Fatalf("content should be omitted for a post delivery: %#v", body)
	}
	post, ok := body["post"].(map[string]any)
	if !ok || post["zh_cn"] == nil {
		t.Fatalf("post payload = %#v, want zh_cn locale", body["post"])
	}
}
