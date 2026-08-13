package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	taskSilentExitTickInterval = time.Minute
	taskSilentExitGracePeriod  = 90 * time.Second
	taskSilentExitBatchSize    = 100
)

type taskSilentExitQueries interface {
	ListSilentExitMonitorWorkspaces(context.Context) ([]db.ListSilentExitMonitorWorkspacesRow, error)
	ListSilentExitCandidates(context.Context, db.ListSilentExitCandidatesParams) ([]db.ListSilentExitCandidatesRow, error)
}

type taskSilentExitDelivery interface {
	Push(context.Context, lark.ProactivePushParams) (lark.ProactivePushResult, error)
}

type taskSilentExitMonitorSettings struct {
	Enabled         bool   `json:"enabled"`
	Timezone        string `json:"timezone"`
	StartTime       string `json:"start_time"`
	EndTime         string `json:"end_time"`
	IntervalMinutes int    `json:"interval_minutes"`
	EnabledAt       string `json:"enabled_at"`
}

type taskSilentExitWorkspaceSettings struct {
	Monitor taskSilentExitMonitorSettings `json:"task_silent_exit_monitor"`
}

type parsedTaskSilentExitSchedule struct {
	enabledAt       time.Time
	location        *time.Location
	startMinute     int
	endMinute       int
	intervalMinutes int
}

// TaskSilentExitMonitor detects completed issue tasks that were expected to
// close out but produced neither a successor task nor a machine-recognisable
// user-wait/final notification. It only reports; it never changes issue state
// or starts another agent task.
type TaskSilentExitMonitor struct {
	queries  taskSilentExitQueries
	delivery taskSilentExitDelivery
	now      func() time.Time

	mu       sync.Mutex
	lastSlot map[string]string
}

func NewTaskSilentExitMonitor(queries *db.Queries, delivery *lark.DeliveryService) *TaskSilentExitMonitor {
	monitor := &TaskSilentExitMonitor{
		queries: queries, now: time.Now,
		lastSlot: make(map[string]string),
	}
	if delivery != nil {
		monitor.delivery = delivery
	}
	return monitor
}

func (m *TaskSilentExitMonitor) Run(ctx context.Context) {
	if m == nil || m.queries == nil || m.delivery == nil {
		return
	}
	m.sweep(ctx)
	ticker := time.NewTicker(taskSilentExitTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sweep(ctx)
		}
	}
}

func (m *TaskSilentExitMonitor) sweep(ctx context.Context) {
	now := m.now().UTC()
	workspaces, err := m.queries.ListSilentExitMonitorWorkspaces(ctx)
	if err != nil {
		slog.Warn("task silent-exit monitor: list workspaces failed", "error", err)
		return
	}
	for _, workspace := range workspaces {
		schedule, err := parseTaskSilentExitSchedule(workspace.Settings, now)
		if err != nil {
			slog.Warn("task silent-exit monitor: invalid workspace schedule",
				"workspace_id", util.UUIDToString(workspace.ID), "error", err)
			continue
		}
		slot, active := schedule.slot(now)
		if !active || !m.claimSlot(util.UUIDToString(workspace.ID), slot) {
			continue
		}
		m.sweepWorkspace(ctx, workspace.ID, schedule, now)
	}
}

func (m *TaskSilentExitMonitor) claimSlot(workspaceID, slot string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastSlot[workspaceID] == slot {
		return false
	}
	m.lastSlot[workspaceID] = slot
	return true
}

func (m *TaskSilentExitMonitor) sweepWorkspace(ctx context.Context, workspaceID pgtype.UUID, schedule parsedTaskSilentExitSchedule, now time.Time) {
	completedBefore := now.Add(-taskSilentExitGracePeriod)
	if !schedule.enabledAt.Before(completedBefore) {
		return
	}
	candidates, err := m.queries.ListSilentExitCandidates(ctx, db.ListSilentExitCandidatesParams{
		WorkspaceID:     workspaceID,
		CompletedAfter:  pgtype.Timestamptz{Time: schedule.enabledAt, Valid: true},
		CompletedBefore: pgtype.Timestamptz{Time: completedBefore, Valid: true},
		CandidateLimit:  taskSilentExitBatchSize,
	})
	if err != nil {
		slog.Warn("task silent-exit monitor: list candidates failed",
			"workspace_id", util.UUIDToString(workspaceID), "error", err)
		return
	}
	for _, candidate := range candidates {
		m.notifyCandidate(ctx, candidate)
	}
}

func (m *TaskSilentExitMonitor) notifyCandidate(ctx context.Context, candidate db.ListSilentExitCandidatesRow) {
	issueIdentifier := candidate.IssuePrefix + "-" + strconv.Itoa(int(candidate.IssueNumber))
	taskID := util.UUIDToString(candidate.TaskID)
	content := strings.Join([]string{
		"⚠️ 工作流可能已静默停止",
		"",
		fmt.Sprintf("执行者：%s", candidate.AgentName),
		fmt.Sprintf("当前 Issue：%s", issueIdentifier),
		"检测结果：Agent Task 已完成，但没有发现后续 Task、等待人工通知或最终完成通知。",
		fmt.Sprintf("Issue 当前状态：%s（仅供参考，不参与静默判定）", candidate.IssueStatus),
		"",
		"请直接回复本消息，回复会写回当前 Issue 的评论线程并交给原执行者继续处理。",
	}, "\n")
	_, err := m.delivery.Push(ctx, lark.ProactivePushParams{
		WorkspaceID:     candidate.WorkspaceID,
		AgentID:         candidate.AgentID,
		IssueID:         candidate.IssueID,
		SourceTaskID:    candidate.TaskID,
		ParentCommentID: candidate.ParentCommentID,
		Content:         content,
		IdempotencyKey:  "task-watchdog:silent-exit:" + taskID,
		ReplyPolicy:     "issue_route",
	})
	if err != nil {
		slog.Warn("task silent-exit monitor: Feishu alert failed",
			"workspace_id", util.UUIDToString(candidate.WorkspaceID),
			"issue_id", util.UUIDToString(candidate.IssueID),
			"task_id", util.UUIDToString(candidate.TaskID),
			"error", err)
	}
}

func parseTaskSilentExitSchedule(raw []byte, now time.Time) (parsedTaskSilentExitSchedule, error) {
	var settings taskSilentExitWorkspaceSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return parsedTaskSilentExitSchedule{}, fmt.Errorf("decode settings: %w", err)
	}
	cfg := settings.Monitor
	if !cfg.Enabled {
		return parsedTaskSilentExitSchedule{}, fmt.Errorf("monitor is disabled")
	}
	location, err := time.LoadLocation(strings.TrimSpace(cfg.Timezone))
	if err != nil {
		return parsedTaskSilentExitSchedule{}, fmt.Errorf("invalid timezone: %w", err)
	}
	startMinute, err := parseClockMinute(cfg.StartTime)
	if err != nil {
		return parsedTaskSilentExitSchedule{}, fmt.Errorf("invalid start_time: %w", err)
	}
	endMinute, err := parseClockMinute(cfg.EndTime)
	if err != nil {
		return parsedTaskSilentExitSchedule{}, fmt.Errorf("invalid end_time: %w", err)
	}
	if cfg.IntervalMinutes < 1 || cfg.IntervalMinutes > 1440 {
		return parsedTaskSilentExitSchedule{}, fmt.Errorf("interval_minutes must be between 1 and 1440")
	}
	enabledAt, err := time.Parse(time.RFC3339, cfg.EnabledAt)
	if err != nil {
		// A manually-created legacy setting should not back-scan the entire
		// workspace. Restrict its first pass to one configured interval.
		enabledAt = now.Add(-time.Duration(cfg.IntervalMinutes) * time.Minute)
	}
	return parsedTaskSilentExitSchedule{
		enabledAt: enabledAt.UTC(), location: location,
		startMinute: startMinute, endMinute: endMinute,
		intervalMinutes: cfg.IntervalMinutes,
	}, nil
}

func parseClockMinute(value string) (int, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, err
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func (s parsedTaskSilentExitSchedule) slot(now time.Time) (string, bool) {
	local := now.In(s.location)
	minute := local.Hour()*60 + local.Minute()
	var elapsed int
	switch {
	case s.startMinute == s.endMinute:
		elapsed = (minute - s.startMinute + 1440) % 1440
	case s.startMinute < s.endMinute:
		if minute < s.startMinute || minute >= s.endMinute {
			return "", false
		}
		elapsed = minute - s.startMinute
	default:
		if minute >= s.startMinute {
			elapsed = minute - s.startMinute
		} else if minute < s.endMinute {
			elapsed = 1440 - s.startMinute + minute
		} else {
			return "", false
		}
	}
	slotStart := local.Add(-time.Duration(elapsed%s.intervalMinutes) * time.Minute)
	return slotStart.Format("2006-01-02T15:04Z07:00"), true
}
