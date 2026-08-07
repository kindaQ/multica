package lark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type ProactivePushParams struct {
	WorkspaceID    pgtype.UUID
	InstallationID pgtype.UUID
	AgentID        pgtype.UUID
	IssueID        pgtype.UUID
	Content        string
	Post           json.RawMessage
	IdempotencyKey string
	ReplyPolicy    string
}

type ProactivePushResult struct {
	InstallationID pgtype.UUID
	DeliveryID     pgtype.UUID
	MessageID      string
	Duplicate      bool
}

var (
	ErrNoAccessibleInstallation = errors.New("no active Feishu Bot is available to this agent")
	ErrAmbiguousInstallations   = errors.New("multiple active Feishu Bots are available")
)

// DeliveryService is the single guarded entry point for agent-initiated
// Feishu messages. The recipient is derived from the installation owner; a
// caller can never supply an arbitrary open_id.
type DeliveryService struct {
	q       *db.Queries
	store   *ChannelStore
	install *InstallationService
	client  APIClient
}

func NewDeliveryService(q *db.Queries, store *ChannelStore, install *InstallationService, client APIClient) *DeliveryService {
	return &DeliveryService{q: q, store: store, install: install, client: client}
}

func (s *DeliveryService) Push(ctx context.Context, p ProactivePushParams) (ProactivePushResult, error) {
	if s == nil || s.q == nil || s.store == nil || s.install == nil || s.client == nil {
		return ProactivePushResult{}, errors.New("feishu delivery service is not configured")
	}
	p.Content = strings.TrimSpace(p.Content)
	p.IdempotencyKey = strings.TrimSpace(p.IdempotencyKey)
	hasContent := p.Content != ""
	hasPost := len(p.Post) > 0
	if hasContent == hasPost {
		return ProactivePushResult{}, errors.New("exactly one of content or post is required")
	}
	if p.IdempotencyKey == "" || !p.WorkspaceID.Valid || !p.AgentID.Valid {
		return ProactivePushResult{}, errors.New("workspace, agent, message, and idempotency key are required")
	}
	if hasPost {
		if len(p.Post) > 20*1024 {
			return ProactivePushResult{}, errors.New("Feishu post exceeds 20KB")
		}
		var err error
		p.Content, err = flattenOutboundPost(p.Post)
		if err != nil {
			return ProactivePushResult{}, err
		}
	}
	if p.ReplyPolicy == "" {
		if p.IssueID.Valid {
			p.ReplyPolicy = "issue_route"
		} else {
			p.ReplyPolicy = "chat_route"
		}
	}
	if p.ReplyPolicy != "disabled" && p.ReplyPolicy != "chat_route" && p.ReplyPolicy != "issue_route" {
		return ProactivePushResult{}, errors.New("invalid reply policy")
	}
	if p.ReplyPolicy == "issue_route" && !p.IssueID.Valid {
		return ProactivePushResult{}, errors.New("issue_route requires an issue")
	}

	var inst Installation
	var err error
	if p.InstallationID.Valid {
		inst, err = s.store.GetLarkInstallationInWorkspace(ctx, GetInstallationInWorkspaceParams{ID: p.InstallationID, WorkspaceID: p.WorkspaceID})
		if err != nil {
			return ProactivePushResult{}, fmt.Errorf("load installation: %w", err)
		}
	} else {
		installations, listErr := s.store.ListActiveLarkInstallationsAccessibleToAgent(ctx, p.WorkspaceID, p.AgentID)
		if listErr != nil {
			return ProactivePushResult{}, fmt.Errorf("discover Feishu installation: %w", listErr)
		}
		inst, err = selectSingleAccessibleInstallation(installations)
		if err != nil {
			return ProactivePushResult{}, err
		}
		p.InstallationID = inst.ID
	}
	if InstallationStatus(inst.Status) != InstallationActive {
		return ProactivePushResult{}, errors.New("feishu installation is not active")
	}
	agent, err := s.q.GetAgent(ctx, p.AgentID)
	if err != nil || agent.WorkspaceID != p.WorkspaceID || agent.ArchivedAt.Valid {
		return ProactivePushResult{}, errors.New("calling agent is unavailable in this workspace")
	}
	allowed := inst.TargetType == string(InstallationTargetAgent) && inst.TargetID == p.AgentID
	if inst.TargetType == string(InstallationTargetSquad) {
		squad, squadErr := s.q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: inst.TargetID, WorkspaceID: p.WorkspaceID})
		if squadErr == nil && !squad.ArchivedAt.Valid {
			allowed = squad.LeaderID == p.AgentID
			if !allowed {
				allowed, err = s.q.IsSquadMember(ctx, db.IsSquadMemberParams{SquadID: squad.ID, MemberType: "agent", MemberID: p.AgentID})
				if err != nil {
					return ProactivePushResult{}, fmt.Errorf("check squad membership: %w", err)
				}
			}
		}
	}
	if !allowed {
		return ProactivePushResult{}, errors.New("calling agent is outside the installation squad")
	}
	if p.IssueID.Valid {
		issue, issueErr := s.q.GetIssue(ctx, p.IssueID)
		if issueErr != nil || issue.WorkspaceID != p.WorkspaceID {
			return ProactivePushResult{}, errors.New("issue is unavailable in this workspace")
		}
	}
	binding, err := s.q.GetChannelUserBindingForMulticaUser(ctx, db.GetChannelUserBindingForMulticaUserParams{
		InstallationID: inst.ID, MulticaUserID: inst.InstallerUserID,
	})
	if err != nil {
		return ProactivePushResult{}, fmt.Errorf("installation owner has no Feishu identity binding: %w", err)
	}
	routeType := "chat"
	if p.IssueID.Valid {
		routeType = "issue"
	}
	delivery, err := s.q.GetProactiveDeliveryByKey(ctx, db.GetProactiveDeliveryByKeyParams{
		InstallationID: inst.ID, RequestKey: textOrNull(p.IdempotencyKey),
	})
	var sourceCommentID pgtype.UUID
	if errors.Is(err, pgx.ErrNoRows) {
		var chatSessionID pgtype.UUID
		if p.IssueID.Valid {
			comment, commentErr := s.q.CreateComment(ctx, db.CreateCommentParams{
				IssueID: p.IssueID, WorkspaceID: p.WorkspaceID, AuthorType: "agent",
				AuthorID: p.AgentID, Content: p.Content, Type: "comment",
			})
			if commentErr != nil {
				return ProactivePushResult{}, fmt.Errorf("persist proactive issue comment: %w", commentErr)
			}
			sourceCommentID = comment.ID
		} else {
			previous, previousErr := s.q.GetLatestProactiveChatDelivery(ctx, db.GetLatestProactiveChatDeliveryParams{
				InstallationID: inst.ID, AgentID: p.AgentID,
			})
			if previousErr == nil {
				chatSessionID = previous.ChatSessionID
			} else if errors.Is(previousErr, pgx.ErrNoRows) {
				session, sessionErr := s.q.CreateChatSession(ctx, db.CreateChatSessionParams{
					WorkspaceID: p.WorkspaceID, AgentID: p.AgentID, CreatorID: inst.InstallerUserID,
					Title: proactiveChatTitle(agent.Name),
				})
				if sessionErr != nil {
					return ProactivePushResult{}, fmt.Errorf("create proactive chat session: %w", sessionErr)
				}
				chatSessionID = session.ID
			} else {
				return ProactivePushResult{}, fmt.Errorf("load proactive chat session: %w", previousErr)
			}
			if _, messageErr := s.q.CreateChatMessage(ctx, db.CreateChatMessageParams{
				ChatSessionID: chatSessionID, Role: "assistant", Content: p.Content,
			}); messageErr != nil {
				return ProactivePushResult{}, fmt.Errorf("persist proactive chat message: %w", messageErr)
			}
			_ = s.q.TouchChatSession(ctx, chatSessionID)
		}
		created, createErr := s.q.GetOrCreateProactiveDelivery(ctx, db.GetOrCreateProactiveDeliveryParams{
			WorkspaceID: p.WorkspaceID, InstallationID: inst.ID, ChannelType: channelTypeFeishu,
			RequestKey: textOrNull(p.IdempotencyKey), RouteType: routeType, ReplyPolicy: p.ReplyPolicy,
			ChatSessionID: chatSessionID, IssueID: p.IssueID, AgentID: p.AgentID, SourceUserID: inst.InstallerUserID,
			DestinationChannelUserID: textOrNull(binding.ChannelUserID),
		})
		if createErr != nil {
			return ProactivePushResult{}, fmt.Errorf("create proactive delivery: %w", createErr)
		}
		delivery = proactiveDeliveryRow(created)
	} else if err != nil {
		return ProactivePushResult{}, fmt.Errorf("load proactive delivery: %w", err)
	}
	messages, err := s.q.ListChannelDeliveryMessages(ctx, delivery.ID)
	if err != nil {
		return ProactivePushResult{}, fmt.Errorf("list proactive messages: %w", err)
	}
	for _, message := range messages {
		if message.Status == "sent" && message.ChannelMessageID.Valid {
			return ProactivePushResult{InstallationID: inst.ID, DeliveryID: delivery.ID, MessageID: message.ChannelMessageID.String, Duplicate: true}, nil
		}
	}
	message, err := s.q.CreateChannelDeliveryMessage(ctx, db.CreateChannelDeliveryMessageParams{
		DeliveryID: delivery.ID, SourceCommentID: sourceCommentID, IdempotencyKey: "proactive:" + p.IdempotencyKey,
	})
	if err != nil {
		return ProactivePushResult{}, fmt.Errorf("create proactive message: %w", err)
	}
	secret, err := s.install.DecryptAppSecret(inst)
	if err != nil {
		return ProactivePushResult{}, err
	}
	creds := InstallationCredentials{AppID: inst.AppID, AppSecret: secret, Region: RegionOrDefault(inst.Region)}
	if inst.TenantKey.Valid {
		creds.TenantKey = inst.TenantKey.String
	}
	var messageID string
	if hasPost {
		sender, ok := s.client.(PostMessageSender)
		if !ok {
			return ProactivePushResult{}, errors.New("feishu client does not support rich-text posts")
		}
		messageID, err = sender.SendPostMessage(ctx, SendPostParams{InstallationID: creds, OpenID: OpenID(binding.ChannelUserID), PostJSON: string(p.Post)})
	} else if containsMarkdown(p.Content) {
		messageID, err = s.client.SendMarkdownCard(ctx, SendMarkdownCardParams{InstallationID: creds, OpenID: OpenID(binding.ChannelUserID), Markdown: p.Content})
	} else {
		messageID, err = s.client.SendTextMessage(ctx, SendTextParams{InstallationID: creds, OpenID: OpenID(binding.ChannelUserID), Text: p.Content})
	}
	if err != nil {
		_, _ = s.q.MarkChannelDeliveryMessageFailed(ctx, db.MarkChannelDeliveryMessageFailedParams{ID: message.ID, LastError: textOrNull("send_failed")})
		_, _ = s.q.SetChannelDeliveryStatus(ctx, db.SetChannelDeliveryStatusParams{ID: delivery.ID, Status: "failed", TerminalReason: textOrNull("send_failed")})
		return ProactivePushResult{}, err
	}
	if _, err := s.q.MarkChannelDeliveryMessageSent(ctx, db.MarkChannelDeliveryMessageSentParams{ID: message.ID, ChannelMessageID: textOrNull(messageID)}); err != nil {
		return ProactivePushResult{}, fmt.Errorf("persist proactive message id: %w", err)
	}
	_, _ = s.q.SetChannelDeliveryStatus(ctx, db.SetChannelDeliveryStatusParams{ID: delivery.ID, Status: "sent", TerminalReason: textOrNull("sent")})
	return ProactivePushResult{InstallationID: inst.ID, DeliveryID: delivery.ID, MessageID: messageID}, nil
}

func flattenOutboundPost(raw json.RawMessage) (string, error) {
	var locales map[string]json.RawMessage
	if err := json.Unmarshal(raw, &locales); err != nil || len(locales) == 0 {
		return "", errors.New("Feishu post must be a non-empty locale object")
	}
	keys := make([]string, 0, len(locales))
	for key := range locales {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	selected := locales["zh_cn"]
	if len(selected) == 0 {
		selected = locales[keys[0]]
	}
	content := strings.TrimSpace(flattenPostContent(string(selected)))
	if content == "" {
		return "", errors.New("Feishu post has no readable content")
	}
	return content, nil
}

func proactiveChatTitle(agentName string) string {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return "Feishu proactive chat"
	}
	return agentName + " · Feishu proactive chat"
}

func selectSingleAccessibleInstallation(installations []Installation) (Installation, error) {
	switch len(installations) {
	case 0:
		return Installation{}, ErrNoAccessibleInstallation
	case 1:
		return installations[0], nil
	default:
		ids := make([]string, 0, len(installations))
		for _, installation := range installations {
			ids = append(ids, uuidString(installation.ID))
		}
		return Installation{}, fmt.Errorf("%w; pass --installation-id with one of: %s", ErrAmbiguousInstallations, strings.Join(ids, ", "))
	}
}

func proactiveDeliveryRow(row db.GetOrCreateProactiveDeliveryRow) db.ChannelDelivery {
	return db.ChannelDelivery{
		ID: row.ID, WorkspaceID: row.WorkspaceID, InstallationID: row.InstallationID,
		ChannelType: row.ChannelType, Kind: row.Kind, RequestKey: row.RequestKey,
		RouteType: row.RouteType, ReplyPolicy: row.ReplyPolicy, TaskID: row.TaskID,
		ChatSessionID: row.ChatSessionID, IssueID: row.IssueID, AgentID: row.AgentID,
		SourceUserID: row.SourceUserID, DestinationChannelUserID: row.DestinationChannelUserID,
		DestinationChatID: row.DestinationChatID, DestinationThreadID: row.DestinationThreadID,
		DestinationMessageID: row.DestinationMessageID, Status: row.Status,
		LeaseToken: row.LeaseToken, LeaseExpiresAt: row.LeaseExpiresAt,
		AttemptCount: row.AttemptCount, NextAttemptAt: row.NextAttemptAt,
		TerminalReason: row.TerminalReason, LastError: row.LastError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
