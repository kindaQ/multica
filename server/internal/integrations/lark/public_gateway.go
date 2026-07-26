package lark

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type PublicGatewayConfig struct {
	Queries     *db.Queries
	Bindings    *AccountBindingService
	Client      APIClient
	Credentials CredentialsResolver
	AppURL      string
	Next        channel.InboundHandler
	Logger      *slog.Logger
	Pending     PublicGatewayPendingHandler
	Tx          TxStarter
}

// PublicGatewayPendingHandler receives an addressed group message from an
// unbound sender. U5 supplies the durable public-workspace implementation; nil
// means the message is acknowledged as unconfigured without entering the
// normal agent router.
type PublicGatewayPendingHandler interface {
	StoreUnboundGroupMessage(context.Context, Installation, channel.InboundMessage) error
	TryDispatchReply(context.Context, Installation, channel.InboundMessage) (bool, error)
}

// PublicGateway intercepts only the installation selected in instance_state.
// Ordinary Feishu installations and every other channel continue directly to
// the shared engine Router.
type PublicGateway struct {
	queries     *db.Queries
	store       *ChannelStore
	bindings    *AccountBindingService
	client      APIClient
	credentials CredentialsResolver
	appURL      string
	next        channel.InboundHandler
	logger      *slog.Logger
	pending     PublicGatewayPendingHandler
}

func NewPublicGateway(cfg PublicGatewayConfig) (*PublicGateway, error) {
	if cfg.Queries == nil || cfg.Bindings == nil || cfg.Client == nil || cfg.Credentials == nil || cfg.Next == nil {
		return nil, errors.New("lark public gateway: incomplete dependencies")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	gateway := &PublicGateway{
		queries:     cfg.Queries,
		store:       NewChannelStore(cfg.Queries),
		bindings:    cfg.Bindings,
		client:      cfg.Client,
		credentials: cfg.Credentials,
		appURL:      strings.TrimRight(cfg.AppURL, "/"),
		next:        cfg.Next,
		logger:      cfg.Logger,
		pending:     cfg.Pending,
	}
	if gateway.pending == nil && cfg.Tx != nil {
		gateway.pending = &PendingDispatchService{
			queries: gateway.queries,
			tx:      cfg.Tx,
			gateway: gateway,
		}
	}
	return gateway, nil
}

func (g *PublicGateway) Handle(ctx context.Context, msg channel.InboundMessage) error {
	if msg.Source.ChannelType != channel.TypeFeishu {
		return g.next(ctx, msg)
	}
	lm, err := larkMsgFromRaw(msg)
	if err != nil {
		return g.next(ctx, msg)
	}
	inst, err := g.store.GetLarkInstallationByAppID(ctx, lm.AppID)
	if err != nil {
		return g.next(ctx, msg)
	}
	state, err := g.queries.GetInstanceState(ctx)
	if err != nil || !state.PublicChannelInstallationID.Valid || state.PublicChannelInstallationID != inst.ID {
		return g.next(ctx, msg)
	}
	if state.Status != "ready" {
		return g.replyOnce(ctx, inst, msg, "公共飞书机器人当前已暂停，请稍后再试。")
	}
	if msg.Source.ChatType == channel.ChatTypeGroup && !msg.AddressedToBot {
		// The public installation must never fall through to its protected
		// storage-only Agent. Group events that do not address the bot are
		// intentionally ignored.
		return nil
	}
	if msg.Source.ChatType == channel.ChatTypeGroup && msg.ReplyTo != nil && g.pending != nil {
		handled, err := g.pending.TryDispatchReply(ctx, inst, msg)
		if err != nil || handled {
			return err
		}
	}

	binding, err := g.queries.GetChannelAccountBindingByChannelUser(ctx, db.GetChannelAccountBindingByChannelUserParams{
		InstallationID: inst.ID,
		ChannelUserID:  msg.Source.SenderID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return g.handleUnbound(ctx, inst, msg)
	}
	if err != nil {
		return fmt.Errorf("lark public gateway: resolve account binding: %w", err)
	}

	target, reason, err := g.resolveTarget(ctx, binding.MulticaUserID)
	if err != nil {
		return err
	}
	if reason != "" {
		return g.replyOnce(ctx, inst, msg, reason)
	}
	msg.RouteTarget = &channel.RouteTarget{
		WorkspaceID: util.UUIDToString(target.workspaceID),
		AgentID:     util.UUIDToString(target.agentID),
		UserID:      util.UUIDToString(binding.MulticaUserID),
		BindingKey:  gatewayBindingKey(msg, target.workspaceID, target.agentID),
	}
	return g.next(ctx, msg)
}

func (g *PublicGateway) handleUnbound(ctx context.Context, inst Installation, msg channel.InboundMessage) error {
	if msg.Source.ChatType == channel.ChatTypeGroup {
		if g.pending == nil {
			return g.replyOnce(ctx, inst, msg, "消息暂存功能尚未配置，请稍后再试。")
		}
		return g.pending.StoreUnboundGroupMessage(ctx, inst, msg)
	}
	if g.appURL == "" {
		return errors.New("lark public gateway: app URL is not configured")
	}
	claimed, duplicate, err := g.claim(ctx, inst.ID, msg.MessageID)
	if err != nil || duplicate {
		return err
	}
	token, err := g.bindings.Mint(ctx, inst.ID, OpenID(msg.Source.SenderID), msg.MessageID)
	if err != nil {
		g.release(ctx, inst.ID, msg.MessageID, claimed)
		return fmt.Errorf("lark public gateway: mint account binding token: %w", err)
	}
	creds, err := g.installationCredentials(inst)
	if err != nil {
		g.release(ctx, inst.ID, msg.MessageID, claimed)
		return err
	}
	bindURL := g.appURL + "/lark/bind?mode=account&token=" + url.QueryEscape(token.Raw)
	if err := g.client.SendBindingPromptCard(ctx, BindingPromptParams{
		InstallationID: creds,
		OpenID:         OpenID(msg.Source.SenderID),
		BindURL:        bindURL,
	}); err != nil {
		g.release(ctx, inst.ID, msg.MessageID, claimed)
		return fmt.Errorf("lark public gateway: send account binding prompt: %w", err)
	}
	return g.mark(ctx, inst.ID, msg.MessageID, claimed)
}

type publicGatewayTarget struct {
	workspaceID pgtype.UUID
	agentID     pgtype.UUID
}

func (g *PublicGateway) resolveTarget(ctx context.Context, userID pgtype.UUID) (publicGatewayTarget, string, error) {
	user, err := g.queries.GetUser(ctx, userID)
	if err != nil {
		return publicGatewayTarget{}, "", fmt.Errorf("lark public gateway: load user: %w", err)
	}
	if !user.DefaultWorkspaceID.Valid {
		return publicGatewayTarget{}, "请先在 Multica 中创建或选择默认工作区。", nil
	}
	if _, err := g.queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      userID,
		WorkspaceID: user.DefaultWorkspaceID,
	}); err != nil {
		return publicGatewayTarget{}, "你的默认工作区已失效，请在 Multica 中重新选择。", nil
	}
	setting, err := g.queries.GetWorkspaceChannelSetting(ctx, db.GetWorkspaceChannelSettingParams{
		WorkspaceID: user.DefaultWorkspaceID,
		ChannelType: channelTypeFeishu,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return publicGatewayTarget{}, "默认工作区尚未配置飞书消息处理智能体，请联系工作区管理员。", nil
	}
	if err != nil {
		return publicGatewayTarget{}, "", fmt.Errorf("lark public gateway: load workspace channel setting: %w", err)
	}
	if !setting.DefaultAgentID.Valid {
		return publicGatewayTarget{}, "默认工作区尚未配置飞书消息处理智能体，请联系工作区管理员。", nil
	}
	agent, err := g.queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
		ID:          setting.DefaultAgentID,
		WorkspaceID: user.DefaultWorkspaceID,
	})
	if err != nil || agent.ArchivedAt.Valid {
		return publicGatewayTarget{}, "工作区默认智能体不可用，请联系工作区管理员重新配置。", nil
	}
	targets, err := g.queries.ListAgentInvocationTargets(ctx, agent.ID)
	if err != nil {
		return publicGatewayTarget{}, "", fmt.Errorf("lark public gateway: load agent invocation targets: %w", err)
	}
	invocable := agent.PermissionMode == "public_to"
	if invocable {
		invocable = false
		for _, target := range targets {
			if target.TargetType == "workspace" && target.TargetID == user.DefaultWorkspaceID {
				invocable = true
				break
			}
		}
	}
	if !invocable {
		return publicGatewayTarget{}, "工作区默认智能体不允许工作区成员使用，请联系工作区管理员。", nil
	}
	return publicGatewayTarget{workspaceID: user.DefaultWorkspaceID, agentID: agent.ID}, "", nil
}

func gatewayBindingKey(msg channel.InboundMessage, workspaceID, agentID pgtype.UUID) string {
	workspace := util.UUIDToString(workspaceID)
	agent := util.UUIDToString(agentID)
	if msg.Source.ChatType == channel.ChatTypeP2P {
		return "gateway:p2p:" + msg.Source.SenderID + ":" + workspace + ":" + agent
	}
	thread := msg.Source.ThreadID
	if thread == "" {
		thread = "root"
	}
	return "gateway:group:" + msg.Source.ChatID + ":" + thread + ":" + msg.Source.SenderID + ":" + workspace + ":" + agent
}

func (g *PublicGateway) replyOnce(ctx context.Context, inst Installation, msg channel.InboundMessage, text string) error {
	claim, duplicate, err := g.claim(ctx, inst.ID, msg.MessageID)
	if err != nil || duplicate {
		return err
	}
	creds, err := g.installationCredentials(inst)
	if err != nil {
		g.release(ctx, inst.ID, msg.MessageID, claim)
		return err
	}
	if _, err := g.client.SendTextMessage(ctx, SendTextParams{
		InstallationID: creds,
		ChatID:         ChatID(msg.Source.ChatID),
		Text:           text,
	}); err != nil {
		g.release(ctx, inst.ID, msg.MessageID, claim)
		return err
	}
	return g.mark(ctx, inst.ID, msg.MessageID, claim)
}

func (g *PublicGateway) installationCredentials(inst Installation) (InstallationCredentials, error) {
	secret, err := g.credentials.DecryptAppSecret(inst)
	if err != nil {
		return InstallationCredentials{}, fmt.Errorf("lark public gateway: decrypt app secret: %w", err)
	}
	out := InstallationCredentials{
		AppID:     inst.AppID,
		AppSecret: secret,
		Region:    RegionOrDefault(inst.Region),
	}
	if inst.TenantKey.Valid {
		out.TenantKey = inst.TenantKey.String
	}
	return out, nil
}

func (g *PublicGateway) claim(ctx context.Context, installationID pgtype.UUID, messageID string) (pgtype.UUID, bool, error) {
	if messageID == "" {
		return pgtype.UUID{}, false, nil
	}
	row, err := g.store.ClaimLarkInboundDedup(ctx, ClaimInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, true, nil
	}
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	return row.ClaimToken, false, nil
}

func (g *PublicGateway) mark(ctx context.Context, installationID pgtype.UUID, messageID string, claim pgtype.UUID) error {
	if messageID == "" || !claim.Valid {
		return nil
	}
	_, err := g.store.MarkLarkInboundDedupProcessed(ctx, MarkInboundDedupProcessedParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claim,
	})
	return err
}

func (g *PublicGateway) release(ctx context.Context, installationID pgtype.UUID, messageID string, claim pgtype.UUID) {
	if messageID == "" || !claim.Valid {
		return
	}
	if _, err := g.store.ReleaseLarkInboundDedup(ctx, ReleaseInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claim,
	}); err != nil {
		g.logger.Warn("lark public gateway: release dedup failed", "message_id", messageID, "error", err)
	}
}
