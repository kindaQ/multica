package lark

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const routeContextTTL = 30 * time.Minute

// RouteResolver implements Feishu's slash-command and quoted-message routing.
// It runs after identity binding, so every lookup is scoped by both the
// installation workspace and the authenticated Multica user.
type RouteResolver struct {
	q   *ChannelStore
	now func() time.Time
}

func NewRouteResolver(q *ChannelStore) *RouteResolver {
	return &RouteResolver{q: q, now: time.Now}
}

func (r *RouteResolver) ResolveRoute(ctx context.Context, inst engine.ResolvedInstallation, sender engine.ResolvedIdentity, msg channel.InboundMessage) (engine.RouteResolution, error) {
	result := engine.RouteResolution{Installation: inst}
	commandText := strings.TrimSpace(msg.CommandText)
	if commandText == "" {
		commandText = strings.TrimSpace(msg.Text)
	}
	if isRouteCommand(commandText) {
		return r.handleCommand(ctx, result, sender, msg, commandText)
	}

	if msg.ReplyTo != nil && msg.ReplyTo.MessageID != "" {
		delivery, err := r.q.GetChannelDeliveryRouteByMessageID(ctx, db.GetChannelDeliveryRouteByMessageIDParams{
			InstallationID:   inst.ID,
			ChannelMessageID: textOrNull(msg.ReplyTo.MessageID),
		})
		if err == nil {
			messages, messageErr := r.q.ListChannelDeliveryMessages(ctx, delivery.ID)
			if messageErr != nil {
				return result, fmt.Errorf("list quoted delivery messages: %w", messageErr)
			}
			result.ParentCommentID = quotedDeliverySourceCommentID(messages, msg.ReplyTo.MessageID)
			if delivery.ReplyPolicy == "disabled" {
				result.Handled = true
				result.Message = "这条通知不接受回复；请使用 /route 指定目标。"
				return result, nil
			}
			result.ChatSessionID = delivery.ChatSessionID
			if delivery.ChatSessionID.Valid && delivery.RouteType == "chat" {
				if _, bindingErr := r.q.GetLarkChatSessionBindingBySession(ctx, delivery.ChatSessionID); errors.Is(bindingErr, pgx.ErrNoRows) {
					bindingKey, config := larkSessionRouting(msg, delivery.AgentID)
					if _, createErr := r.q.CreateChannelChatSessionBinding(ctx, db.CreateChannelChatSessionBindingParams{
						ChatSessionID: delivery.ChatSessionID, InstallationID: inst.ID,
						ChannelType: channelTypeFeishu, ChannelChatID: bindingKey,
						ChatType: string(msg.Source.ChatType), Config: config, AgentID: delivery.AgentID,
					}); createErr != nil {
						return result, fmt.Errorf("bind proactive chat session: %w", createErr)
					}
				} else if bindingErr != nil {
					return result, fmt.Errorf("lookup proactive chat binding: %w", bindingErr)
				}
			}
			return r.applyRoute(ctx, result, delivery.IssueID, delivery.AgentID, pgtype.UUID{})
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return result, fmt.Errorf("lookup quoted delivery: %w", err)
		}
		if msg.Source.ChatType == channel.ChatTypeGroup && !msg.AddressedToBot {
			result.Ignored = true
			return result, nil
		}
	}

	selected, err := r.q.GetPendingChannelRouteContext(ctx, db.GetPendingChannelRouteContextParams{
		InstallationID:  inst.ID,
		ConversationKey: routeConversationKey(msg),
		ChannelUserID:   msg.Source.SenderID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("load pending route: %w", err)
	}
	return r.applyRoute(ctx, result, selected.IssueID, selected.AgentID, selected.ID)
}

func quotedDeliverySourceCommentID(messages []db.ChannelDeliveryMessage, channelMessageID string) pgtype.UUID {
	for _, message := range messages {
		if message.Status == "sent" && message.ChannelMessageID.Valid && message.ChannelMessageID.String == channelMessageID {
			return message.SourceCommentID
		}
	}
	return pgtype.UUID{}
}

func (r *RouteResolver) handleCommand(ctx context.Context, result engine.RouteResolution, sender engine.ResolvedIdentity, msg channel.InboundMessage, text string) (engine.RouteResolution, error) {
	result.Handled = true
	cmd, err := parseRouteCommand(text)
	if err != nil {
		result.Message = err.Error() + "\n" + routeHelpText()
		return result, nil
	}

	key := routeConversationKey(msg)
	switch cmd.Action {
	case "help":
		result.Message = routeHelpText()
		return result, nil
	case "cancel":
		_, err := r.q.CancelChannelRouteContext(ctx, db.CancelChannelRouteContextParams{
			InstallationID:  result.Installation.ID,
			ConversationKey: key,
			ChannelUserID:   msg.Source.SenderID,
		})
		if err != nil {
			return result, fmt.Errorf("cancel route: %w", err)
		}
		result.Message = "Route cleared. Messages without a quoted route will go to the squad leader."
		return result, nil
	case "status":
		current, err := r.q.GetPendingChannelRouteContext(ctx, db.GetPendingChannelRouteContextParams{
			InstallationID:  result.Installation.ID,
			ConversationKey: key,
			ChannelUserID:   msg.Source.SenderID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			result.Message = "No pending route. The next message will go to the squad leader."
			return result, nil
		}
		if err != nil {
			return result, fmt.Errorf("get route status: %w", err)
		}
		result.Message = routeSummary(current.IssueID, current.AgentID, current.ExpiresAt.Time)
		return result, nil
	}

	issueID, validationMessage, err := r.resolveIssue(ctx, result.Installation.WorkspaceID, cmd.Issue)
	if err != nil {
		return result, err
	}
	if validationMessage != "" {
		result.Message = validationMessage
		return result, nil
	}
	agentID, validationMessage, err := r.resolveAgent(ctx, result.Installation, cmd.Agent)
	if err != nil {
		return result, err
	}
	if validationMessage != "" {
		result.Message = validationMessage
		return result, nil
	}

	row, err := r.q.UpsertChannelRouteContext(ctx, db.UpsertChannelRouteContextParams{
		WorkspaceID:     result.Installation.WorkspaceID,
		InstallationID:  result.Installation.ID,
		ChannelType:     channelTypeFeishu,
		ConversationKey: key,
		ChannelUserID:   msg.Source.SenderID,
		IssueID:         issueID,
		AgentID:         agentID,
		SourceMessageID: textOrNull(msg.MessageID),
		ExpiresAt:       pgtype.Timestamptz{Time: r.now().Add(routeContextTTL), Valid: true},
	})
	if err != nil {
		return result, fmt.Errorf("save route: %w", err)
	}
	_ = sender // retained in the signature for future per-issue authorization.
	if cmd.Message != "" {
		result.Handled = false
		result, err = r.applyRoute(ctx, result, row.IssueID, row.AgentID, row.ID)
		if err != nil || result.Handled {
			return result, err
		}
		result.InputText = cmd.Message
		return result, nil
	}
	result.Message = routeSummary(row.IssueID, row.AgentID, row.ExpiresAt.Time)
	return result, nil
}

func (r *RouteResolver) applyRoute(ctx context.Context, result engine.RouteResolution, issueID, agentID, routeContextID pgtype.UUID) (engine.RouteResolution, error) {
	if issueID.Valid && !agentID.Valid {
		issue, err := r.q.GetIssue(ctx, issueID)
		if err != nil || issue.WorkspaceID != result.Installation.WorkspaceID {
			result.Handled = true
			result.Message = "The selected issue no longer exists in this workspace. Run /route again."
			return result, nil
		}
		switch issue.AssigneeType.String {
		case "agent":
			agentID = issue.AssigneeID
		case "squad":
			if result.Installation.TargetType != string(InstallationTargetSquad) || issue.AssigneeID != result.Installation.TargetID {
				result.Handled = true
				result.Message = "That issue belongs to another squad. Select an agent from this bot's squad explicitly."
				return result, nil
			}
			squad, err := r.q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: result.Installation.TargetID, WorkspaceID: result.Installation.WorkspaceID})
			if err != nil {
				return result, fmt.Errorf("resolve squad leader: %w", err)
			}
			agentID = squad.LeaderID
		default:
			result.Handled = true
			result.Message = "This issue has no agent or squad assignee. Run /route --issue <issue> --agent <agent>."
			return result, nil
		}
	}
	if agentID.Valid {
		allowed, err := r.agentAllowed(ctx, result.Installation, agentID)
		if err != nil {
			return result, err
		}
		if !allowed {
			result.Handled = true
			result.Message = "The selected agent is no longer in this bot's squad. Run /route again."
			return result, nil
		}
		result.Installation.AgentID = agentID
		if platform, ok := result.Installation.Platform.(Installation); ok {
			platform.AgentID = agentID
			result.Installation.Platform = platform
		}
	}
	result.IssueID = issueID
	result.RouteContextID = routeContextID
	return result, nil
}

func (r *RouteResolver) resolveIssue(ctx context.Context, workspaceID pgtype.UUID, raw string) (pgtype.UUID, string, error) {
	if raw == "" {
		return pgtype.UUID{}, "", nil
	}
	if id, err := uuid.Parse(raw); err == nil {
		issue, qerr := r.q.GetIssue(ctx, pgtype.UUID{Bytes: id, Valid: true})
		if errors.Is(qerr, pgx.ErrNoRows) || (qerr == nil && issue.WorkspaceID != workspaceID) {
			return pgtype.UUID{}, "Issue not found in this workspace.", nil
		}
		if qerr != nil {
			return pgtype.UUID{}, "", qerr
		}
		return issue.ID, "", nil
	}
	part := raw
	if idx := strings.LastIndex(raw, "-"); idx >= 0 {
		part = raw[idx+1:]
	}
	n, err := strconv.ParseInt(part, 10, 32)
	if err != nil || n <= 0 {
		return pgtype.UUID{}, "Issue must be an issue key such as MUL-123 or a UUID.", nil
	}
	issue, err := r.q.GetIssueByNumber(ctx, db.GetIssueByNumberParams{WorkspaceID: workspaceID, Number: int32(n)})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, "Issue not found in this workspace.", nil
	}
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	return issue.ID, "", nil
}

func (r *RouteResolver) resolveAgent(ctx context.Context, inst engine.ResolvedInstallation, raw string) (pgtype.UUID, string, error) {
	if raw == "" {
		return pgtype.UUID{}, "", nil
	}
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "@"), "＠")
	if id, err := uuid.Parse(raw); err == nil {
		agentID := pgtype.UUID{Bytes: id, Valid: true}
		allowed, qerr := r.agentAllowed(ctx, inst, agentID)
		if qerr != nil {
			return pgtype.UUID{}, "", qerr
		}
		if !allowed {
			return pgtype.UUID{}, "Agent not found in this bot's squad.", nil
		}
		return agentID, "", nil
	}
	agents, err := r.q.ListAgents(ctx, inst.WorkspaceID)
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	matches := make([]db.Agent, 0, 1)
	for _, agent := range agents {
		if !strings.EqualFold(strings.TrimSpace(agent.Name), raw) {
			continue
		}
		allowed, allowErr := r.agentAllowed(ctx, inst, agent.ID)
		if allowErr != nil {
			return pgtype.UUID{}, "", allowErr
		}
		if allowed {
			matches = append(matches, agent)
		}
	}
	if len(matches) == 0 {
		return pgtype.UUID{}, "Agent not found in this bot's squad.", nil
	}
	if len(matches) > 1 {
		return pgtype.UUID{}, "Agent name is ambiguous. Use the agent UUID.", nil
	}
	return matches[0].ID, "", nil
}

func (r *RouteResolver) agentAllowed(ctx context.Context, inst engine.ResolvedInstallation, agentID pgtype.UUID) (bool, error) {
	agent, err := r.q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: inst.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if agent.ArchivedAt.Valid {
		return false, nil
	}
	if inst.TargetType != string(InstallationTargetSquad) {
		return agent.ID == inst.TargetID, nil
	}
	squad, err := r.q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: inst.TargetID, WorkspaceID: inst.WorkspaceID})
	if err != nil {
		return false, err
	}
	if squad.LeaderID == agentID {
		return true, nil
	}
	return r.q.IsSquadMember(ctx, db.IsSquadMemberParams{SquadID: squad.ID, MemberType: "agent", MemberID: agentID})
}

type routeCommand struct {
	Action  string
	Issue   string
	Agent   string
	Message string
}

var routeTokenPattern = regexp.MustCompile(`\S+`)

func isRouteCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "/route")
}

func parseRouteCommand(text string) (routeCommand, error) {
	spans := routeTokenPattern.FindAllStringIndex(text, -1)
	token := func(index int) string {
		return text[spans[index][0]:spans[index][1]]
	}
	if len(spans) == 0 || !strings.EqualFold(token(0), "/route") {
		return routeCommand{}, errors.New("not a route command")
	}
	if len(spans) == 1 {
		return routeCommand{Action: "help"}, nil
	}
	if len(spans) == 2 {
		action := strings.ToLower(token(1))
		if action == "help" || action == "status" || action == "cancel" {
			return routeCommand{Action: action}, nil
		}
	}
	cmd := routeCommand{Action: "set"}
	for i := 1; i < len(spans); i++ {
		switch token(i) {
		case "--issue":
			if i+1 >= len(spans) || strings.HasPrefix(token(i+1), "--") {
				return routeCommand{}, errors.New("--issue requires a value")
			}
			i++
			cmd.Issue = token(i)
		case "--agent":
			if i+1 >= len(spans) || strings.HasPrefix(token(i+1), "--") {
				return routeCommand{}, errors.New("--agent requires a value")
			}
			i++
			cmd.Agent = token(i)
		case "--":
			if i+1 < len(spans) {
				cmd.Message = strings.TrimSpace(text[spans[i+1][0]:])
			}
			i = len(spans)
		default:
			if strings.HasPrefix(token(i), "--") {
				return routeCommand{}, fmt.Errorf("unknown route argument %q", token(i))
			}
			cmd.Message = strings.TrimSpace(text[spans[i][0]:])
			i = len(spans)
		}
	}
	if cmd.Issue == "" && cmd.Agent == "" {
		return routeCommand{}, errors.New("specify --issue, --agent, or both")
	}
	return cmd, nil
}

func routeConversationKey(msg channel.InboundMessage) string {
	if msg.Source.ThreadID != "" {
		return msg.Source.ChatID + ":" + msg.Source.ThreadID
	}
	return msg.Source.ChatID
}

func routeSummary(issueID, agentID pgtype.UUID, expiresAt time.Time) string {
	parts := make([]string, 0, 2)
	if issueID.Valid {
		parts = append(parts, "issue="+uuidString(issueID))
	}
	if agentID.Valid {
		parts = append(parts, "agent="+uuidString(agentID))
	}
	return "Route set for the next accepted message: " + strings.Join(parts, ", ") + ". Expires at " + expiresAt.Local().Format("15:04") + "."
}

func routeHelpText() string {
	return strings.Join([]string{
		"Route this message, or omit message text to route the next one:",
		"/route --issue MUL-123 --agent AgentName message text",
		"/route --issue MUL-123 --agent AgentName",
		"/route --issue MUL-123",
		"/route --agent AgentName",
		"/route status",
		"/route cancel",
	}, "\n")
}
