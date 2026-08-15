package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeTaskSilentExitQueries struct {
	workspaces []db.ListSilentExitMonitorWorkspacesRow
	candidates []db.ListSilentExitCandidatesRow
	params     []db.ListSilentExitCandidatesParams
}

func (f *fakeTaskSilentExitQueries) ListSilentExitMonitorWorkspaces(context.Context) ([]db.ListSilentExitMonitorWorkspacesRow, error) {
	return f.workspaces, nil
}

func (f *fakeTaskSilentExitQueries) ListSilentExitCandidates(_ context.Context, params db.ListSilentExitCandidatesParams) ([]db.ListSilentExitCandidatesRow, error) {
	f.params = append(f.params, params)
	return f.candidates, nil
}

type fakeTaskSilentExitDelivery struct {
	pushes []lark.ProactivePushParams
}

func (f *fakeTaskSilentExitDelivery) Push(_ context.Context, params lark.ProactivePushParams) (lark.ProactivePushResult, error) {
	f.pushes = append(f.pushes, params)
	return lark.ProactivePushResult{}, nil
}

func silentExitTestUUID(last byte) pgtype.UUID {
	var value [16]byte
	value[15] = last
	return pgtype.UUID{Bytes: value, Valid: true}
}

func monitorSettingsJSON(t *testing.T, cfg taskSilentExitMonitorSettings) []byte {
	t.Helper()
	raw, err := json.Marshal(taskSilentExitWorkspaceSettings{Monitor: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTaskSilentExitScheduleWindows(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		start, end  int
		now         time.Time
		wantActive  bool
		wantSlotEnd string
	}{
		{
			name: "day window", start: 9 * 60, end: 22 * 60,
			now:        time.Date(2026, 8, 13, 10, 7, 0, 0, location),
			wantActive: true, wantSlotEnd: "10:00+08:00",
		},
		{
			name: "outside day window", start: 9 * 60, end: 22 * 60,
			now:        time.Date(2026, 8, 13, 22, 0, 0, 0, location),
			wantActive: false,
		},
		{
			name: "overnight window", start: 22 * 60, end: 8 * 60,
			now:        time.Date(2026, 8, 14, 0, 17, 0, 0, location),
			wantActive: true, wantSlotEnd: "00:10+08:00",
		},
		{
			name: "equal times mean all day", start: 9 * 60, end: 9 * 60,
			now:        time.Date(2026, 8, 13, 3, 2, 0, 0, location),
			wantActive: true, wantSlotEnd: "03:00+08:00",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schedule := parsedTaskSilentExitSchedule{
				location: location, startMinute: tt.start, endMinute: tt.end, intervalMinutes: 10,
			}
			slot, active := schedule.slot(tt.now)
			if active != tt.wantActive {
				t.Fatalf("active = %v, want %v", active, tt.wantActive)
			}
			if tt.wantSlotEnd != "" && slot[len(slot)-len(tt.wantSlotEnd):] != tt.wantSlotEnd {
				t.Fatalf("slot = %q, want suffix %q", slot, tt.wantSlotEnd)
			}
		})
	}
}

func TestTaskSilentExitMonitorSendsIssueRoutedAlertOncePerSlot(t *testing.T) {
	now := time.Date(2026, 8, 13, 2, 5, 0, 0, time.UTC) // 10:05 Asia/Shanghai
	workspaceID, issueID, agentID, taskID, parentID := silentExitTestUUID(1), silentExitTestUUID(2), silentExitTestUUID(3), silentExitTestUUID(4), silentExitTestUUID(5)
	queries := &fakeTaskSilentExitQueries{
		workspaces: []db.ListSilentExitMonitorWorkspacesRow{{
			ID: workspaceID,
			Settings: monitorSettingsJSON(t, taskSilentExitMonitorSettings{
				Enabled: true, Timezone: "Asia/Shanghai", StartTime: "09:00", EndTime: "22:00",
				IntervalMinutes: 10, EnabledAt: now.Add(-time.Hour).Format(time.RFC3339),
			}),
		}},
		candidates: []db.ListSilentExitCandidatesRow{{
			TaskID: taskID, WorkspaceID: workspaceID, IssueID: issueID, AgentID: agentID,
			CompletedAt: pgtype.Timestamptz{Time: time.Date(2026, 8, 13, 1, 59, 0, 0, time.UTC), Valid: true},
			IssueStatus: "blocked", IssueTitle: "指标中心需求6", IssueNumber: 43, IssuePrefix: "PEN",
			WorkspaceSlug: "pengqiang", AgentName: "developer", ParentCommentID: parentID,
			LastProgress: "已完成需求理解。\n当前缺少原型图。",
			LastProgressAt: pgtype.Timestamptz{
				Time: time.Date(2026, 8, 13, 1, 23, 0, 0, time.UTC), Valid: true,
			},
		}},
	}
	delivery := &fakeTaskSilentExitDelivery{}
	monitor := &TaskSilentExitMonitor{
		queries: queries, delivery: delivery, appURL: "http://multica.example", now: func() time.Time { return now }, lastSlot: make(map[string]string),
	}

	monitor.sweep(context.Background())
	monitor.sweep(context.Background())

	if len(queries.params) != 1 {
		t.Fatalf("candidate scans = %d, want 1", len(queries.params))
	}
	if len(delivery.pushes) != 1 {
		t.Fatalf("pushes = %d, want 1", len(delivery.pushes))
	}
	push := delivery.pushes[0]
	if push.ReplyPolicy != "issue_route" || push.IssueID != issueID || push.AgentID != agentID {
		t.Fatalf("unexpected route: %+v", push)
	}
	if push.ParentCommentID != parentID || push.SourceTaskID != taskID {
		t.Fatalf("unexpected comment lineage: %+v", push)
	}
	if push.IdempotencyKey != "task-watchdog:silent-exit:00000000-0000-0000-0000-000000000004" {
		t.Fatalf("idempotency key = %q", push.IdempotencyKey)
	}
	for _, want := range []string{
		"⚠️ 工作流可能异常停止，请关注",
		"[PEN-43｜指标中心需求6](http://multica.example/pengqiang/issues/00000000-0000-0000-0000-000000000002)",
		"Task 完成时间：2026-08-13 09:59",
		"> 已完成需求理解。",
		"> 当前缺少原型图。",
		"该评论发布于 09:23。Task 随后继续运行约 36 分钟，并于 09:59 完成，但没有再发布结果或后续交接。",
		"请直接引用本消息回复",
	} {
		if !strings.Contains(push.Content, want) {
			t.Fatalf("content missing %q:\n%s", want, push.Content)
		}
	}
}

func TestSilentExitProgressLimitsOnlyQuotedBody(t *testing.T) {
	content := strings.Repeat("进", taskSilentExitProgressRunes+50)
	got := silentExitProgress(content)
	if len([]rune(got)) != taskSilentExitProgressRunes {
		t.Fatalf("progress runes = %d, want %d", len([]rune(got)), taskSilentExitProgressRunes)
	}
	if !strings.HasSuffix(got, strings.TrimPrefix(taskSilentExitTruncatedText, "\n")) {
		t.Fatalf("truncation marker missing: %q", got[len(got)-40:])
	}
	if quoted := quoteMarkdown("第一行\n\n第三行"); quoted != "> 第一行\n>\n> 第三行" {
		t.Fatalf("quoted markdown = %q", quoted)
	}
}

func TestSilentExitDetectionResultMatchesCandidateKind(t *testing.T) {
	if got := silentExitDetectionResult("user_reply"); !strings.Contains(got, "用户的飞书回复") || !strings.Contains(got, "没有闭环") {
		t.Fatalf("user reply result = %q", got)
	}
	if got := silentExitDetectionResult("workflow_handoff"); !strings.Contains(got, "没有发现后续 Task") || !strings.Contains(got, "工作流可能停在了这里") {
		t.Fatalf("workflow handoff result = %q", got)
	}
}
