package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateComment_TriggeredTaskRejectsTopLevelComment exercises the full
// CreateComment handler path (not just taskCoversReplyParent) for the trap
// reported in MUL-4417 / GH #5266: a comment-triggered task that posts a
// parentless, top-level comment on its own issue is rejected with a 409 whose
// message names the trigger comment and states that top-level comments are not
// allowed. Pinning the message here keeps it from silently drifting away from
// the behavior the CLI help now documents.
func TestCreateComment_TriggeredTaskRejectsTopLevelComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)

	w := httptest.NewRecorder()
	r := newRequest("POST", "/api/issues/"+fx.IssueID+"/comments", map[string]any{
		"content": "dispatching a squad from this task",
	})
	r = withURLParam(r, "id", fx.IssueID)
	r.Header.Set("X-Agent-ID", fx.LeaderID)
	r.Header.Set("X-Task-ID", fx.TaskID)

	testHandler.CreateComment(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("CreateComment top-level: expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if got := countAgentCommentsForIssue(t, fx.IssueID, fx.LeaderID); got != 0 {
		t.Fatalf("expected rejected top-level comment not to be stored, got %d", got)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	msg, _ := body["error"].(string)
	// Pin the three semantic pieces without locking the exact wording: why it
	// was rejected, the comment to reply under, and the actionable fix. The last
	// one guards against the guidance being dropped in a future edit.
	for _, want := range []string{
		"top-level comments",   // reason
		fx.TriggerCommentID,    // the comment to reply under
		"parent_id (--parent)", // actionable fix
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("409 message should contain %q, got %q", want, msg)
		}
	}
}

// TestCreateComment_TriggeredTaskAllowsReplyUnderTrigger is the positive half:
// the same task replying under its trigger comment succeeds, proving the guard
// rejects only the top-level case and does not lock the whole issue for
// comments (MUL-4417 / GH #5266).
func TestCreateComment_TriggeredTaskAllowsReplyUnderTrigger(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)

	w := httptest.NewRecorder()
	r := newRequest("POST", "/api/issues/"+fx.IssueID+"/comments", map[string]any{
		"content":   "replying under the trigger comment",
		"parent_id": fx.TriggerCommentID,
	})
	r = withURLParam(r, "id", fx.IssueID)
	r.Header.Set("X-Agent-ID", fx.LeaderID)
	r.Header.Set("X-Task-ID", fx.TaskID)

	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment reply-under-trigger: expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// An issue-routed Feishu push already stores the exact outbound body as the
// task's result comment. If the agent then follows the generic comment step and
// posts a delivery receipt, CreateComment must reuse the existing result rather
// than creating a second user-visible comment.
func TestCreateComment_ReusesSentProactiveFeishuResult(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)
	ctx := context.Background()
	var resultCommentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (
			issue_id, workspace_id, author_type, author_id, content, type,
			parent_id, source_task_id
		) VALUES ($1, $2, 'agent', $3, 'fruit joke', 'comment', $4, $5)
		RETURNING id
	`, fx.IssueID, testWorkspaceID, fx.LeaderID, fx.TriggerCommentID, fx.TaskID).Scan(&resultCommentID); err != nil {
		t.Fatalf("create proactive result comment: %v", err)
	}

	var deliveryID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO channel_delivery (
			workspace_id, installation_id, channel_type, kind, route_type,
			reply_policy, issue_id, agent_id, destination_channel_user_id, status
		) VALUES ($1, gen_random_uuid(), 'feishu', 'proactive_push', 'issue',
			'issue_route', $2, $3, 'ou_test', 'sent')
		RETURNING id
	`, testWorkspaceID, fx.IssueID, fx.LeaderID).Scan(&deliveryID); err != nil {
		t.Fatalf("create proactive delivery: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM channel_delivery_message WHERE delivery_id = $1`, deliveryID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM channel_delivery WHERE id = $1`, deliveryID)
	})
	if _, err := testPool.Exec(ctx, `
		INSERT INTO channel_delivery_message (
			delivery_id, source_comment_id, idempotency_key, channel_message_id,
			status, sent_at
		) VALUES ($1, $2, 'fruit-joke', 'om_fruit_joke', 'sent', now())
	`, deliveryID, resultCommentID); err != nil {
		t.Fatalf("create proactive delivery message: %v", err)
	}

	w := httptest.NewRecorder()
	r := newRequest("POST", "/api/issues/"+fx.IssueID+"/comments", map[string]any{
		"content":   "Sent through Feishu (message_id: om_fruit_joke)",
		"parent_id": fx.TriggerCommentID,
	})
	r = withURLParam(r, "id", fx.IssueID)
	r.Header.Set("X-Agent-ID", fx.LeaderID)
	r.Header.Set("X-Task-ID", fx.TaskID)

	testHandler.CreateComment(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("CreateComment receipt replay: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Multica-Comment-Reused"); got != "feishu-proactive-result" {
		t.Fatalf("reuse header = %q", got)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode reused comment: %v", err)
	}
	if body["id"] != resultCommentID || body["content"] != "fruit joke" {
		t.Fatalf("expected original proactive comment, got %+v", body)
	}
	var count int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM comment
		WHERE issue_id = $1 AND source_task_id = $2 AND author_type = 'agent'
	`, fx.IssueID, fx.TaskID).Scan(&count); err != nil {
		t.Fatalf("count task comments: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one task comment after receipt replay, got %d", count)
	}
}

// TestCreateComment_TriggeredTaskRejectsForeignParent covers the resumed-session
// drift in GH #6264: the task passes a --parent that is a real comment on its
// own issue but not one this run was given to answer. The refusal must name
// both the parent it rejected and the parent to use — and must NOT say a
// top-level comment was attempted, since that wording sent agents looking for a
// new-thread opt-in instead of correcting the --parent they already passed.
func TestCreateComment_TriggeredTaskRejectsForeignParent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)

	// Must be a real comment on the same issue: a nonexistent id, or one from
	// another issue, is refused earlier with a 400 and never reaches this guard.
	var foreignParentID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'member', $3, 'an earlier thread this task never owned', 'comment')
		RETURNING id
	`, fx.IssueID, testWorkspaceID, testUserID).Scan(&foreignParentID); err != nil {
		t.Fatalf("create foreign parent comment: %v", err)
	}

	w := httptest.NewRecorder()
	r := newRequest("POST", "/api/issues/"+fx.IssueID+"/comments", map[string]any{
		"content":   "posting under a parent carried over from a previous turn",
		"parent_id": foreignParentID,
	})
	r = withURLParam(r, "id", fx.IssueID)
	r.Header.Set("X-Agent-ID", fx.LeaderID)
	r.Header.Set("X-Task-ID", fx.TaskID)

	testHandler.CreateComment(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("CreateComment foreign parent: expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if got := countAgentCommentsForIssue(t, fx.IssueID, fx.LeaderID); got != 0 {
		t.Fatalf("expected rejected comment not to be stored, got %d", got)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	msg, _ := body["error"].(string)
	for _, want := range []string{
		foreignParentID,        // the parent that was refused
		fx.TriggerCommentID,    // the parent to use instead
		"parent_id (--parent)", // actionable fix
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("409 message should contain %q, got %q", want, msg)
		}
	}
	if strings.Contains(msg, "top-level") {
		t.Fatalf("409 message must not claim a top-level comment was attempted, got %q", msg)
	}
}
