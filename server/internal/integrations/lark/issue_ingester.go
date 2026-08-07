package lark

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// IssueIngester owns the atomic issue-route boundary. Remote sends and event
// publication happen only after the database transaction commits.
type IssueIngester struct {
	q     *db.Queries
	tx    engine.TxStarter
	tasks *service.TaskService
	bus   *events.Bus
}

func NewIssueIngester(q *db.Queries, tx engine.TxStarter, tasks *service.TaskService, bus *events.Bus) *IssueIngester {
	return &IssueIngester{q: q, tx: tx, tasks: tasks, bus: bus}
}

func (i *IssueIngester) IngestIssueMessage(ctx context.Context, p engine.IssueIngressParams) (engine.IssueIngressResult, error) {
	if i == nil || i.q == nil || i.tx == nil || i.tasks == nil {
		return engine.IssueIngressResult{}, errors.New("lark issue ingester is not configured")
	}
	if !p.IssueID.Valid || !p.Installation.AgentID.Valid || !p.Sender.UserID.Valid {
		return engine.IssueIngressResult{}, errors.New("issue, agent, and sender are required")
	}
	tx, err := i.tx.Begin(ctx)
	if err != nil {
		return engine.IssueIngressResult{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := i.q.WithTx(tx)

	issue, err := qtx.GetIssue(ctx, p.IssueID)
	if err != nil {
		return engine.IssueIngressResult{}, fmt.Errorf("load issue: %w", err)
	}
	if issue.WorkspaceID != p.Installation.WorkspaceID {
		return engine.IssueIngressResult{}, errors.New("issue does not belong to installation workspace")
	}
	parentCommentID := p.ParentCommentID
	if parentCommentID.Valid {
		parent, parentErr := qtx.GetComment(ctx, parentCommentID)
		switch {
		case errors.Is(parentErr, pgx.ErrNoRows):
			parentCommentID = pgtype.UUID{}
		case parentErr != nil:
			return engine.IssueIngressResult{}, fmt.Errorf("load reply parent comment: %w", parentErr)
		case parent.IssueID != issue.ID || parent.WorkspaceID != issue.WorkspaceID:
			parentCommentID = pgtype.UUID{}
		}
	}
	content := issueInboundCommentContent(p.Message)
	comment, err := qtx.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		AuthorType:  "member",
		AuthorID:    p.Sender.UserID,
		Content:     content,
		Type:        "comment",
		ParentID:    parentCommentID,
	})
	if err != nil {
		return engine.IssueIngressResult{}, fmt.Errorf("create member comment: %w", err)
	}

	task, err := i.tasks.EnqueueTaskForMentionWithQueries(ctx, qtx, issue, p.Installation.AgentID, comment.ID)
	if err != nil {
		return engine.IssueIngressResult{}, fmt.Errorf("create explicit issue task: %w", err)
	}
	if _, err := qtx.CreateChannelDelivery(ctx, db.CreateChannelDeliveryParams{
		WorkspaceID:          issue.WorkspaceID,
		InstallationID:       p.Installation.ID,
		ChannelType:          channelTypeFeishu,
		Kind:                 "conversation_reply",
		RouteType:            "issue",
		ReplyPolicy:          "issue_route",
		TaskID:               task.ID,
		IssueID:              issue.ID,
		AgentID:              p.Installation.AgentID,
		SourceUserID:         p.Sender.UserID,
		DestinationChatID:    textOrNull(p.Message.Source.ChatID),
		DestinationThreadID:  textOrNull(p.Message.Source.ThreadID),
		DestinationMessageID: textOrNull(p.Message.MessageID),
	}); err != nil {
		return engine.IssueIngressResult{}, fmt.Errorf("create delivery intent: %w", err)
	}

	if p.ClaimToken.Valid && p.Message.MessageID != "" {
		rows, err := qtx.MarkChannelInboundDedupProcessed(ctx, db.MarkChannelInboundDedupProcessedParams{
			InstallationID: p.Installation.ID,
			MessageID:      p.Message.MessageID,
			ClaimToken:     p.ClaimToken,
		})
		if err != nil {
			return engine.IssueIngressResult{}, fmt.Errorf("mark dedup: %w", err)
		}
		if rows == 0 {
			return engine.IssueIngressResult{}, engine.ErrClaimLost
		}
	}
	if p.RouteContextID.Valid {
		if _, err := qtx.ConsumeChannelRouteContext(ctx, p.RouteContextID); err != nil {
			return engine.IssueIngressResult{}, fmt.Errorf("consume route context: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return engine.IssueIngressResult{}, fmt.Errorf("commit: %w", err)
	}

	i.publishCommentCreated(issue, comment)
	i.tasks.PublishTaskQueued(ctx, task)
	return engine.IssueIngressResult{CommentID: comment.ID, TaskID: task.ID, DedupMarked: p.ClaimToken.Valid}, nil
}

func issueInboundCommentContent(message channel.InboundMessage) string {
	if content := strings.TrimSpace(message.CommandText); content != "" {
		return content
	}
	return message.Text
}

func (i *IssueIngester) publishCommentCreated(issue db.Issue, comment db.Comment) {
	if i.bus == nil {
		return
	}
	i.bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   "member",
		ActorID:     util.UUIDToString(comment.AuthorID),
		Payload: map[string]any{
			"comment": map[string]any{
				"id":          util.UUIDToString(comment.ID),
				"issue_id":    util.UUIDToString(comment.IssueID),
				"author_type": comment.AuthorType,
				"author_id":   util.UUIDToString(comment.AuthorID),
				"content":     comment.Content,
				"type":        comment.Type,
				"parent_id":   util.UUIDToPtr(comment.ParentID),
				"created_at":  comment.CreatedAt.Time.UTC().Format(time.RFC3339),
			},
			"issue_title":         issue.Title,
			"issue_assignee_type": issue.AssigneeType,
			"issue_assignee_id":   issue.AssigneeID,
			"issue_status":        issue.Status,
		},
	})
}

var _ engine.IssueIngester = (*IssueIngester)(nil)
