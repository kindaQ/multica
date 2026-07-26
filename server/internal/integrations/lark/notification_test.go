package lark

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNotificationEventEnabled(t *testing.T) {
	raw, _ := json.Marshal([]string{"issue.done", "issue.blocked"})
	if !notificationEventEnabled(raw, "issue.done") {
		t.Fatal("issue.done should be enabled")
	}
	if notificationEventEnabled(raw, "dispatch.completed") {
		t.Fatal("dispatch.completed should not be enabled")
	}
	if notificationEventEnabled([]byte(`{}`), "issue.done") {
		t.Fatal("invalid event payload must be disabled")
	}
}

func TestRenderIssueNotification(t *testing.T) {
	raw := []byte(`{"issue":{"identifier":"MUL-42","title":"Ship public gateway","status":"done"}}`)
	got, err := renderIssueNotification("issue.done", raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "MUL-42 已完成") || !strings.Contains(got, "Ship public gateway") {
		t.Fatalf("unexpected notification: %q", got)
	}
}
