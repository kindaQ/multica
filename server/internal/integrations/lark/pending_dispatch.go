package lark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// PendingDispatchService persists unbound group messages in the protected
// public workspace and recognizes explicit reply-plus-mention dispatch
// commands. It never invokes the public agent.
type PendingDispatchService struct {
	queries *db.Queries
	tx      TxStarter
	gateway *PublicGateway
}

func (s *PendingDispatchService) StoreUnboundGroupMessage(ctx context.Context, inst Installation, msg channel.InboundMessage) error {
	claim, duplicate, err := s.gateway.claim(ctx, inst.ID, msg.MessageID)
	if err != nil || duplicate {
		return err
	}
	release := true
	defer func() {
		if release {
			s.gateway.release(ctx, inst.ID, msg.MessageID, claim)
		}
	}()

	instance, err := s.queries.GetInstanceState(ctx)
	if err != nil || !instance.PublicWorkspaceID.Valid || !instance.PublicAgentID.Valid {
		return errors.New("lark pending dispatch: public workspace is not initialized")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("lark pending dispatch: begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.queries.WithTx(tx)

	thread := pgtype.Text{String: msg.Source.ThreadID, Valid: msg.Source.ThreadID != ""}
	sessionID, err := qtx.GetPublicChatSessionForChannel(ctx, db.GetPublicChatSessionForChannelParams{
		InstallationID:  inst.ID,
		ChannelChatID:   msg.Source.ChatID,
		ChannelThreadID: thread,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		session, createErr := qtx.CreateChatSession(ctx, db.CreateChatSessionParams{
			WorkspaceID:  instance.PublicWorkspaceID,
			AgentID:      instance.PublicAgentID,
			CreatorID:    instance.SuperAdminUserID,
			Title:        "Feishu pending dispatch",
			IsAgentIntro: false,
			ProjectID:    pgtype.UUID{},
		})
		if createErr != nil {
			return fmt.Errorf("lark pending dispatch: create public chat session: %w", createErr)
		}
		sessionID = session.ID
	} else if err != nil {
		return fmt.Errorf("lark pending dispatch: find public chat session: %w", err)
	}

	body := strings.TrimSpace(msg.Text)
	if body == "" {
		body = "[unsupported Feishu message]"
	}
	if _, err := qtx.CreateChatMessage(ctx, db.CreateChatMessageParams{
		ChatSessionID: sessionID,
		Role:          "user",
		Content:       body,
	}); err != nil {
		return fmt.Errorf("lark pending dispatch: persist public message: %w", err)
	}
	sourcePayload := msg.Raw
	if len(sourcePayload) == 0 {
		sourcePayload = json.RawMessage(`{}`)
	}
	if _, err := qtx.CreateChannelPendingDispatch(ctx, db.CreateChannelPendingDispatchParams{
		InstallationID:      inst.ID,
		ChannelType:         channelTypeFeishu,
		PublicWorkspaceID:   instance.PublicWorkspaceID,
		PublicChatSessionID: sessionID,
		ChannelChatID:       msg.Source.ChatID,
		ChannelThreadID:     thread,
		ChannelMessageID:    msg.MessageID,
		SenderChannelUserID: msg.Source.SenderID,
		Content:             body,
		SourcePayload:       sourcePayload,
	}); err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			return fmt.Errorf("lark pending dispatch: persist queue row: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("lark pending dispatch: commit: %w", err)
	}
	if err := s.gateway.mark(ctx, inst.ID, msg.MessageID, claim); err != nil {
		return err
	}
	release = false
	return s.sendGroupReply(ctx, inst, msg, "消息已暂存。回复这条消息并同时 @ 公共机器人和目标成员，即可分发。")
}

func (s *PendingDispatchService) TryDispatchReply(ctx context.Context, inst Installation, msg channel.InboundMessage) (bool, error) {
	if msg.ReplyTo == nil || msg.ReplyTo.MessageID == "" {
		return false, nil
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return true, fmt.Errorf("lark pending dispatch: begin dispatch transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.queries.WithTx(tx)

	// Lock the queue row before routing. Two different group replies can race
	// against the same pending message; holding this lock through enqueue makes
	// exactly one of them the dispatcher. The constant synthetic MessageID used
	// below is a second idempotency guard if the process crashes after enqueue
	// but before this transaction commits.
	pending, err := qtx.GetPendingDispatchByChannelMessage(ctx, db.GetPendingDispatchByChannelMessageParams{
		InstallationID:   inst.ID,
		ChannelMessageID: msg.ReplyTo.MessageID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("lark pending dispatch: load replied message: %w", err)
	}
	claim, duplicate, err := s.gateway.claim(ctx, inst.ID, msg.MessageID)
	if err != nil || duplicate {
		return true, err
	}
	reply := func(text string) (bool, error) {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			s.gateway.release(ctx, inst.ID, msg.MessageID, claim)
			return true, fmt.Errorf("lark pending dispatch: release dispatch lock: %w", rollbackErr)
		}
		return true, s.replyAndFinish(ctx, inst, msg, claim, text)
	}
	if pending.Status != "pending" {
		return reply("这条消息已经分发，无需重复操作。")
	}

	lm, err := larkMsgFromRaw(msg)
	if err != nil {
		s.gateway.release(ctx, inst.ID, msg.MessageID, claim)
		return true, err
	}
	targetOpenIDs := dispatchTargetMentions(lm.Mentions, inst.BotOpenID)
	if len(targetOpenIDs) != 1 {
		text := "请只 @ 一位目标成员。"
		if len(targetOpenIDs) == 0 {
			text = "请在回复中 @ 一位已绑定 Multica 的目标成员。"
		}
		return reply(text)
	}
	targetOpenID := targetOpenIDs[0]
	targetBinding, err := s.queries.GetChannelAccountBindingByChannelUser(ctx, db.GetChannelAccountBindingByChannelUserParams{
		InstallationID: inst.ID,
		ChannelUserID:  targetOpenID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return reply("目标成员尚未绑定 Multica，暂时无法分发。")
	}
	if err != nil {
		s.gateway.release(ctx, inst.ID, msg.MessageID, claim)
		return true, err
	}
	target, reason, err := s.gateway.resolveTarget(ctx, targetBinding.MulticaUserID)
	if err != nil {
		s.gateway.release(ctx, inst.ID, msg.MessageID, claim)
		return true, err
	}
	if reason != "" {
		return reply("无法分发给该成员：" + reason)
	}

	routedRaw := InboundMessage{
		EventType:    "multica.pending_dispatch",
		EventID:      msg.EventID,
		AppID:        inst.AppID,
		ChatID:       ChatID(targetOpenID),
		ChatType:     ChatTypeP2P,
		MessageID:    "dispatch:" + util.UUIDToString(pending.ID),
		SenderOpenID: OpenID(targetOpenID),
		Body:         pending.Content,
		CommandBody:  pending.Content,
		MessageType:  "text",
	}
	raw, _ := json.Marshal(routedRaw)
	routed := channel.InboundMessage{
		EventID:   msg.EventID,
		MessageID: routedRaw.MessageID,
		Source: channel.Source{
			ChannelType: channel.TypeFeishu,
			ChatID:      targetOpenID,
			ChatType:    channel.ChatTypeP2P,
			SenderID:    targetOpenID,
		},
		Type: channel.MsgTypeText,
		Text: pending.Content,
		RouteTarget: &channel.RouteTarget{
			WorkspaceID:    util.UUIDToString(target.workspaceID),
			AgentID:        util.UUIDToString(target.agentID),
			UserID:         util.UUIDToString(targetBinding.MulticaUserID),
			BindingKey:     "gateway:dispatch:" + util.UUIDToString(pending.ID),
			OutboundOpenID: targetOpenID,
		},
		Raw: raw,
	}
	if err := s.gateway.next(ctx, routed); err != nil {
		s.gateway.release(ctx, inst.ID, msg.MessageID, claim)
		return true, fmt.Errorf("lark pending dispatch: route target message: %w", err)
	}

	dispatcherID := pgtype.UUID{}
	if binding, lookupErr := s.queries.GetChannelAccountBindingByChannelUser(ctx, db.GetChannelAccountBindingByChannelUserParams{
		InstallationID: inst.ID,
		ChannelUserID:  msg.Source.SenderID,
	}); lookupErr == nil {
		dispatcherID = binding.MulticaUserID
	}
	if _, err := qtx.MarkChannelPendingDispatchDispatched(ctx, db.MarkChannelPendingDispatchDispatchedParams{
		DispatchMessageID:         pgtype.Text{String: msg.MessageID, Valid: msg.MessageID != ""},
		DispatchedByChannelUserID: pgtype.Text{String: msg.Source.SenderID, Valid: msg.Source.SenderID != ""},
		DispatchedByMulticaUserID: dispatcherID,
		TargetChannelUserID:       pgtype.Text{String: targetOpenID, Valid: true},
		TargetMulticaUserID:       targetBinding.MulticaUserID,
		TargetWorkspaceID:         target.workspaceID,
		TargetAgentID:             target.agentID,
		ID:                        pending.ID,
	}); err != nil {
		s.gateway.release(ctx, inst.ID, msg.MessageID, claim)
		return true, fmt.Errorf("lark pending dispatch: mark dispatched: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		s.gateway.release(ctx, inst.ID, msg.MessageID, claim)
		return true, fmt.Errorf("lark pending dispatch: commit dispatch: %w", err)
	}
	return true, s.replyAndFinish(ctx, inst, msg, claim, "消息已分发，后续处理进展会通过私聊通知目标成员。")
}

func dispatchTargetMentions(mentions []InboundMention, botOpenID string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(mentions))
	for _, mention := range mentions {
		id := strings.TrimSpace(mention.OpenID)
		if id == "" || id == botOpenID {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (s *PendingDispatchService) replyAndFinish(ctx context.Context, inst Installation, msg channel.InboundMessage, claim pgtype.UUID, text string) error {
	if err := s.sendGroupReply(ctx, inst, msg, text); err != nil {
		s.gateway.release(ctx, inst.ID, msg.MessageID, claim)
		return err
	}
	return s.gateway.mark(ctx, inst.ID, msg.MessageID, claim)
}

func (s *PendingDispatchService) sendGroupReply(ctx context.Context, inst Installation, msg channel.InboundMessage, text string) error {
	creds, err := s.gateway.installationCredentials(inst)
	if err != nil {
		return err
	}
	_, err = s.gateway.client.SendTextMessage(ctx, SendTextParams{
		InstallationID: creds,
		ChatID:         ChatID(msg.Source.ChatID),
		Text:           text,
		ReplyTarget:    ReplyTarget{MessageID: msg.MessageID},
	})
	return err
}
