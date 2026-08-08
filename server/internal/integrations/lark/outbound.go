package lark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CardStatus mirrors lark_outbound_card_message.status. Kept as a typed
// alias so callers can't pass arbitrary strings into the status column.
type CardStatus string

const (
	CardStatusPending   CardStatus = "pending"
	CardStatusStreaming CardStatus = "streaming"
	CardStatusFinal     CardStatus = "final"
	CardStatusError     CardStatus = "error"
)

// CardKind enumerates the small set of card variants the patcher
// renders. The Renderer is plug-replaceable so the on-wire card
// template can evolve without touching the patcher's transport / DB
// logic.
type CardKind string

const (
	CardKindThinking CardKind = "thinking"
	CardKindRunning  CardKind = "running"
	CardKindFinal    CardKind = "final"
	CardKindError    CardKind = "error"
)

// CardRender is the rendered card body the Renderer produces. The
// patcher serializes the JSON before handing it to APIClient.
type CardRender struct {
	JSON string
}

// RenderInput is the (typed) snapshot the Renderer sees when building
// or patching a card. Fields are populated as they become available
// during a task lifecycle — IssueNumber is set for `/issue` flows,
// Content is set for completed chat tasks, ErrorMessage for failed.
type RenderInput struct {
	Kind         CardKind
	AgentName    string
	IssueNumber  int32
	IssueID      pgtype.UUID
	TaskID       pgtype.UUID
	Content      string
	ErrorMessage string
}

// Renderer turns a typed RenderInput into the actual Lark card JSON.
// Centralizing this lets us swap card templates (or A/B them) without
// touching event subscription or persistence code.
type Renderer interface {
	Render(in RenderInput) (CardRender, error)
}

// defaultRenderer produces minimal text-only cards that work against
// Lark's generic interactive-card schema. The exact JSON layout will
// be refined when the real product card design lands; this default
// keeps the wiring real (the JSON deserializes against Lark's schema)
// without committing the product to a particular template.
type defaultRenderer struct{}

// NewDefaultRenderer returns the production-default Renderer. Override
// via PatcherConfig.Renderer when a custom template is needed.
func NewDefaultRenderer() Renderer { return &defaultRenderer{} }

func (defaultRenderer) Render(in RenderInput) (CardRender, error) {
	header := "Multica"
	if in.AgentName != "" {
		header = in.AgentName
	}
	var body string
	switch in.Kind {
	case CardKindThinking:
		body = "Thinking…"
	case CardKindRunning:
		body = "Working on it…"
	case CardKindFinal:
		body = in.Content
		if body == "" {
			body = "Done."
		}
	case CardKindError:
		body = "Run failed."
		if in.ErrorMessage != "" {
			body = "Run failed: " + in.ErrorMessage
		}
	default:
		return CardRender{}, fmt.Errorf("unknown card kind %q", in.Kind)
	}
	// update_multi MUST be true on every render: Lark refuses to apply
	// PatchInteractiveCard to a card whose config does not declare it
	// a "shared, updatable" card. Since this renderer drives the
	// thinking → streaming → final/error lifecycle (the card is sent
	// once and patched multiple times), an absent update_multi causes
	// every patch after the first send to silently no-op on the
	// Lark side while the local outbound status row still flips to
	// streaming/final. Keep this on every kind — including thinking
	// and error — because that initial JSON IS the body Lark stores
	// and consults for subsequent patches.
	doc := map[string]any{
		"config": map[string]any{
			"wide_screen_mode": true,
			"update_multi":     true,
		},
		"header": map[string]any{
			"template": "blue",
			"title":    map[string]any{"tag": "plain_text", "content": header},
		},
		"elements": []any{
			map[string]any{
				"tag": "div",
				"text": map[string]any{
					"tag":     "plain_text",
					"content": body,
				},
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return CardRender{}, err
	}
	return CardRender{JSON: string(raw)}, nil
}

// PatcherQueries is the narrow subset of *db.Queries the Patcher
// needs. Declared as an interface so the patcher is unit-testable
// without a real Postgres connection.
type PatcherQueries interface {
	GetAgentTask(ctx context.Context, id pgtype.UUID) (db.AgentTaskQueue, error)
	TaskHasChannelIngestedMessages(ctx context.Context, taskID pgtype.UUID) (bool, error)
	GetChatSession(ctx context.Context, id pgtype.UUID) (db.ChatSession, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	GetLarkInstallation(ctx context.Context, id pgtype.UUID) (Installation, error)
	GetLarkChatSessionBindingBySession(ctx context.Context, chatSessionID pgtype.UUID) (ChatSessionBinding, error)
	GetLarkOutboundCardByTask(ctx context.Context, taskID pgtype.UUID) (OutboundCardMessage, error)
	CreateLarkOutboundCardMessage(ctx context.Context, arg CreateOutboundCardMessageParams) (OutboundCardMessage, error)
	UpdateLarkOutboundCardStatus(ctx context.Context, arg UpdateOutboundCardStatusParams) error
}

// deliveryQueries is optional so older embedders and focused unit-test fakes
// keep the chat-only contract. The production ChannelStore implements it via
// its embedded generated queries.
type deliveryQueries interface {
	GetOrCreateChannelDelivery(context.Context, db.GetOrCreateChannelDeliveryParams) (db.GetOrCreateChannelDeliveryRow, error)
	ListChannelDeliveriesByTask(context.Context, pgtype.UUID) ([]db.ChannelDelivery, error)
	ListChannelDeliveryMessages(context.Context, pgtype.UUID) ([]db.ChannelDeliveryMessage, error)
	CreateChannelDeliveryMessage(context.Context, db.CreateChannelDeliveryMessageParams) (db.ChannelDeliveryMessage, error)
	MarkChannelDeliveryMessageSent(context.Context, db.MarkChannelDeliveryMessageSentParams) (int64, error)
	MarkChannelDeliveryMessageFailed(context.Context, db.MarkChannelDeliveryMessageFailedParams) (int64, error)
	SetChannelDeliveryStatus(context.Context, db.SetChannelDeliveryStatusParams) (int64, error)
}

type issueFooterQueries interface {
	GetIssue(context.Context, pgtype.UUID) (db.Issue, error)
	GetWorkspace(context.Context, pgtype.UUID) (db.Workspace, error)
}

// CredentialsResolver decrypts an installation's app_secret for the
// transport layer. *InstallationService satisfies it directly; tests
// substitute a fake.
type CredentialsResolver interface {
	DecryptAppSecret(inst Installation) (string, error)
}

// PatcherConfig tunes the outbound Patcher. Defaults via withDefaults;
// tests typically override Renderer / Now / Logger.
type PatcherConfig struct {
	// Renderer drives the error card template used on the EventTaskFailed
	// path. The success path (EventChatDone) bypasses the renderer
	// entirely — it sends the raw assistant reply as a plain text IM
	// message — so this only matters for the failure branch.
	Renderer Renderer
	Now      func() time.Time
	Logger   *slog.Logger
}

func (c PatcherConfig) withDefaults() PatcherConfig {
	if c.Renderer == nil {
		c.Renderer = NewDefaultRenderer()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Patcher reacts to task-lifecycle events on the event bus and forwards
// chat replies to Lark as plain text IM messages. It is the outbound
// side of §4.5 — but the original "thinking → streaming → final card"
// lifecycle was reduced to a single plain-text reply on EventChatDone
// after Bohan reported the card chrome made replies feel like system
// notifications. The error path is the one survivor of card rendering:
// failed runs surface as a short error card on EventTaskFailed because
// the visual distinction from a normal reply is genuinely useful.
//
// Scope:
//
//   - Only tasks whose chat_session has a lark_chat_session_binding
//     produce outbound. Tasks born from the web UI or autopilot pass
//     through unchanged.
//
//   - Each EventChatDone yields one Lark text message; there is no
//     streaming, no throttling, no DB row to track card-state.
//
//   - Multi-replica safety is inherited from the inbound WS lease: at
//     most one replica holds the installation lease at a time, the
//     event bus is per-process, so exactly one Patcher reacts per run.
type Patcher struct {
	queries         PatcherQueries
	credentials     CredentialsResolver
	client          APIClient
	typingIndicator *TypingIndicatorManager
	cfg             PatcherConfig
}

// NewPatcher constructs a Patcher bound to its dependencies. The
// patcher does not subscribe to the bus until Register is called.
func NewPatcher(queries PatcherQueries, credentials CredentialsResolver, client APIClient, cfg PatcherConfig) *Patcher {
	cfg = cfg.withDefaults()
	return &Patcher{
		queries:     queries,
		credentials: credentials,
		client:      client,
		cfg:         cfg,
	}
}

// SetTypingIndicatorManager wires the typing-indicator manager into the
// patcher so that replies clear the "processing" reaction before they
// are sent. Call once at boot after both the patcher and manager are
// constructed. Nil disables the clear step.
func (p *Patcher) SetTypingIndicatorManager(m *TypingIndicatorManager) {
	p.typingIndicator = m
}

// Register subscribes the patcher to the task-lifecycle events it
// cares about on the supplied bus. Idempotent only if you call it
// against a fresh bus; call sites should invoke it exactly once
// during server boot (after the bus + patcher are constructed and
// before HTTP traffic starts).
//
// Subscriptions are deliberately minimal:
//
//   - EventChatDone — the agent finished replying. The Patcher sends
//     the reply as a plain text IM message (Lark's `msg_type=text`),
//     not as an interactive card. The earlier card-based design (with
//     thinking → running → final patches) made every reply look like
//     a system notification nested in card chrome; flipping to plain
//     text makes free-form chat feel native.
//
//   - EventTaskFailed — the run failed; surface a short error card
//     so the failure is visually distinct from a successful reply.
//
// We deliberately do NOT subscribe to EventTaskQueued / EventTaskRunning
// (no thinking-card lifecycle anymore — adds noise without value) or to
// EventTaskCompleted (chat tasks always emit EventChatDone first, which
// is what we care about; non-chat tasks have no Lark binding anyway and
// would early-return). Leaving EventTaskCompleted unsubscribed also
// avoids the prior "Done." overwrite regression where the no-content
// EventTaskCompleted payload would wipe the real reply.
func (p *Patcher) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventTaskFailed, p.handleEvent)
	bus.Subscribe(protocol.EventTaskCancelled, p.handleEvent)
	bus.Subscribe(protocol.EventTaskCompleted, p.handleEvent)
	bus.Subscribe(protocol.EventCommentCreated, p.handleEvent)
	bus.Subscribe(protocol.EventReactionAdded, p.handleEvent)
	bus.Subscribe(protocol.EventChatDone, p.handleEvent)
}

func (p *Patcher) handleEvent(e events.Event) {
	// Use a fresh background ctx with a tight timeout: bus delivery is
	// synchronous so a stuck Lark HTTP call would otherwise wedge the
	// whole publish call site.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.processEvent(ctx, e); err != nil {
		p.cfg.Logger.Warn("lark patcher: event handling failed",
			"event_type", e.Type,
			"task_id", e.TaskID,
			"chat_session_id", e.ChatSessionID,
			"error", err,
		)
	}
}

func (p *Patcher) processEvent(ctx context.Context, e events.Event) error {
	if e.Type == protocol.EventCommentCreated {
		return p.processIssueComment(ctx, e)
	}
	if e.Type == protocol.EventReactionAdded {
		return p.processIssueReaction(ctx, e)
	}
	taskID, chatSessionID, ok := taskAndSessionFromEvent(e)
	if !ok {
		return nil
	}
	if !chatSessionID.Valid {
		return p.processIssueTerminal(ctx, taskID, e)
	}
	binding, err := p.queries.GetLarkChatSessionBindingBySession(ctx, chatSessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Web-only chat session — not a Lark target.
			return nil
		}
		return fmt.Errorf("lookup chat session binding: %w", err)
	}

	// Only bound sessions reach here, so classify the task origin before
	// spending any send work. Web/mobile direct-chat tasks can reuse a session
	// that originated in Lark, but their replies belong only in Multica.
	// Sealed channel tasks own an input batch just like direct tasks, so the
	// discriminator is the immutable channel_ingested provenance of that
	// batch, not chat_input_task_id presence (which #5645 originally used).
	task, err := p.queries.GetAgentTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("load agent task: %w", err)
	}
	deliver, err := engine.TaskInputIsChannelIngested(ctx, p.queries, task)
	if err != nil {
		return fmt.Errorf("classify task input origin: %w", err)
	}
	if !deliver {
		return nil
	}

	inst, err := p.queries.GetLarkInstallation(ctx, binding.InstallationID)
	if err != nil {
		return fmt.Errorf("load installation: %w", err)
	}
	if InstallationStatus(inst.Status) != InstallationActive {
		// Revoked between trigger and event; nothing to patch.
		return nil
	}
	creds, err := p.installationCredentials(inst)
	if err != nil {
		return err
	}

	agent, agentErr := p.queries.GetAgent(ctx, task.AgentID)
	agentName := ""
	if agentErr == nil {
		agentName = agent.Name
	}

	// Clear the "processing" reaction before the reply is visible so the
	// user sees a clean transition. Best-effort: a failure here is logged
	// but does not block the actual reply.
	if p.typingIndicator != nil {
		p.typingIndicator.Clear(ctx, chatSessionID)
	}

	switch e.Type {
	case protocol.EventChatDone:
		if dq, ok := p.queries.(deliveryQueries); ok {
			content := chatDoneContent(e.Payload)
			if content == "" {
				return nil
			}
			delivery, deliveryErr := p.ensureChatDelivery(ctx, dq, inst, binding, taskID)
			if deliveryErr != nil {
				return deliveryErr
			}
			return p.sendDeliveryMessage(ctx, dq, delivery, pgtype.UUID{}, "chat:done", content, proactiveMessageFooter(agentName, ""))
		}
		return p.sendChatReply(ctx, creds, binding, e.Payload, proactiveMessageFooter(agentName, ""))
	case protocol.EventTaskFailed:
		return p.fail(ctx, creds, binding, taskID, agentName, e.Payload)
	}
	return nil
}

func (p *Patcher) processIssueReaction(ctx context.Context, e events.Event) error {
	if e.ActorType != "agent" {
		return nil
	}
	taskID := pgtype.UUID{}
	if err := taskID.Scan(e.TaskID); err != nil || !taskID.Valid {
		return nil
	}
	root, ok := e.Payload.(map[string]any)
	if !ok {
		return nil
	}
	reaction, ok := root["reaction"].(map[string]any)
	if !ok {
		return nil
	}
	emojiType, ok := feishuReactionType(stringValue(reaction["emoji"]))
	if !ok {
		return fmt.Errorf("unsupported Feishu reaction emoji %q", stringValue(reaction["emoji"]))
	}

	dq, ok := p.queries.(deliveryQueries)
	if !ok {
		return nil
	}
	deliveries, err := dq.ListChannelDeliveriesByTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("list reaction deliveries: %w", err)
	}
	for _, delivery := range deliveries {
		if delivery.ChannelType != channelTypeFeishu || delivery.RouteType != "issue" || delivery.Status == "cancelled" || !delivery.DestinationMessageID.Valid {
			continue
		}
		inst, err := p.queries.GetLarkInstallation(ctx, delivery.InstallationID)
		if err != nil {
			return fmt.Errorf("load reaction installation: %w", err)
		}
		if InstallationStatus(inst.Status) != InstallationActive {
			continue
		}
		creds, err := p.installationCredentials(inst)
		if err != nil {
			return err
		}
		if _, err := p.client.AddMessageReaction(ctx, AddReactionParams{
			InstallationID: creds,
			MessageID:      delivery.DestinationMessageID.String,
			EmojiType:      emojiType,
		}); err != nil {
			return fmt.Errorf("add Feishu result reaction: %w", err)
		}
		if _, err := dq.SetChannelDeliveryStatus(ctx, db.SetChannelDeliveryStatusParams{
			ID: delivery.ID, Status: "sent", TerminalReason: textOrNull("reaction"),
		}); err != nil {
			return fmt.Errorf("mark reaction delivery sent: %w", err)
		}
	}
	return nil
}

// Feishu reactions use platform enum names rather than Unicode. Keep the
// model-facing choices intentionally small so the reaction shown in Multica
// can be mirrored exactly instead of being approximated by another face.
func feishuReactionType(emoji string) (string, bool) {
	types := map[string]string{
		"👍":  "THUMBSUP",
		"😂":  "LAUGH",
		"😊":  "SMILE",
		"❤️": "HEART",
		"❤":  "HEART",
		"👏":  "APPLAUSE",
		"🎉":  "PARTY",
	}
	t, ok := types[strings.TrimSpace(emoji)]
	return t, ok
}

func (p *Patcher) ensureChatDelivery(ctx context.Context, dq deliveryQueries, inst Installation, binding ChatSessionBinding, taskID pgtype.UUID) (db.ChannelDelivery, error) {
	row, err := dq.GetOrCreateChannelDelivery(ctx, db.GetOrCreateChannelDeliveryParams{
		WorkspaceID: inst.WorkspaceID, InstallationID: inst.ID, ChannelType: channelTypeFeishu,
		Kind: "conversation_reply", RouteType: "chat", ReplyPolicy: "chat_route",
		TaskID: taskID, ChatSessionID: binding.ChatSessionID, AgentID: binding.AgentID,
		DestinationChatID:   textOrNull(string(outboundChatID(binding))),
		DestinationThreadID: binding.LastThreadID, DestinationMessageID: binding.LastMessageID,
	})
	if err != nil {
		return db.ChannelDelivery{}, fmt.Errorf("get or create chat delivery: %w", err)
	}
	return channelDeliveryFromGetOrCreate(row), nil
}

func channelDeliveryFromGetOrCreate(row db.GetOrCreateChannelDeliveryRow) db.ChannelDelivery {
	return db.ChannelDelivery{
		ID: row.ID, WorkspaceID: row.WorkspaceID, InstallationID: row.InstallationID,
		ChannelType: row.ChannelType, Kind: row.Kind, RouteType: row.RouteType,
		ReplyPolicy: row.ReplyPolicy, TaskID: row.TaskID, ChatSessionID: row.ChatSessionID,
		IssueID: row.IssueID, AgentID: row.AgentID, SourceUserID: row.SourceUserID,
		DestinationChannelUserID: row.DestinationChannelUserID,
		DestinationChatID:        row.DestinationChatID, DestinationThreadID: row.DestinationThreadID,
		DestinationMessageID: row.DestinationMessageID, Status: row.Status,
		LeaseToken: row.LeaseToken, LeaseExpiresAt: row.LeaseExpiresAt,
		AttemptCount: row.AttemptCount, NextAttemptAt: row.NextAttemptAt,
		TerminalReason: row.TerminalReason, LastError: row.LastError,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func (p *Patcher) processIssueComment(ctx context.Context, e events.Event) error {
	dq, ok := p.queries.(deliveryQueries)
	if !ok {
		return nil
	}
	comment, ok := commentDeliveryPayload(e.Payload)
	if !ok || comment.authorType != "agent" || comment.commentType != "comment" || !comment.taskID.Valid {
		return nil
	}
	deliveries, err := dq.ListChannelDeliveriesByTask(ctx, comment.taskID)
	if err != nil {
		return fmt.Errorf("list issue deliveries: %w", err)
	}
	for _, delivery := range deliveries {
		if delivery.ChannelType != channelTypeFeishu || delivery.RouteType != "issue" || delivery.Status == "cancelled" {
			continue
		}
		if delivery.Status == "sent" && delivery.TerminalReason.Valid && delivery.TerminalReason.String == "reaction" {
			continue
		}
		if err := p.sendDeliveryMessage(ctx, dq, delivery, comment.id, "comment:"+uuidString(comment.id), comment.content, p.deliveryFooter(ctx, delivery)); err != nil {
			return err
		}
	}
	return nil
}

func (p *Patcher) processIssueTerminal(ctx context.Context, taskID pgtype.UUID, e events.Event) error {
	dq, ok := p.queries.(deliveryQueries)
	if !ok {
		return nil
	}
	deliveries, err := dq.ListChannelDeliveriesByTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("list terminal deliveries: %w", err)
	}
	for _, delivery := range deliveries {
		if delivery.ChannelType != channelTypeFeishu || delivery.RouteType != "issue" || delivery.Status == "cancelled" {
			continue
		}
		// A reaction-only result is already visible on the user's original
		// Feishu message. Do not turn the subsequent task:completed event into
		// the generic "no visible reply" text message.
		if delivery.Status == "sent" && delivery.TerminalReason.Valid && delivery.TerminalReason.String == "reaction" {
			continue
		}
		status, reason := "sent", "completed"
		var terminalText string
		switch e.Type {
		case protocol.EventTaskFailed:
			status, reason, terminalText = "failed", "failed", "智能体执行失败，请稍后重试。"
		case protocol.EventTaskCancelled:
			status, reason, terminalText = "cancelled", "cancelled", "该任务已取消。"
		case protocol.EventTaskCompleted:
			messages, listErr := dq.ListChannelDeliveryMessages(ctx, delivery.ID)
			if listErr != nil {
				return fmt.Errorf("list delivery messages: %w", listErr)
			}
			hasOutput := false
			for _, message := range messages {
				hasOutput = hasOutput || message.Status == "sent"
			}
			if !hasOutput {
				terminalText = "智能体已完成任务，但没有产生可见回复。"
			}
		default:
			continue
		}
		if terminalText != "" {
			if err := p.sendDeliveryMessage(ctx, dq, delivery, pgtype.UUID{}, "terminal:"+reason, terminalText, ""); err != nil {
				return err
			}
		}
		_, err = dq.SetChannelDeliveryStatus(ctx, db.SetChannelDeliveryStatusParams{
			ID: delivery.ID, Status: status, TerminalReason: textOrNull(reason),
		})
		if err != nil {
			return fmt.Errorf("set delivery terminal status: %w", err)
		}
	}
	return nil
}

type deliveryComment struct {
	id          pgtype.UUID
	taskID      pgtype.UUID
	authorType  string
	commentType string
	content     string
}

func commentDeliveryPayload(payload any) (deliveryComment, bool) {
	root, ok := payload.(map[string]any)
	if !ok {
		return deliveryComment{}, false
	}
	comment, ok := root["comment"].(map[string]any)
	if !ok {
		return deliveryComment{}, false
	}
	var out deliveryComment
	_ = out.id.Scan(stringValue(comment["id"]))
	_ = out.taskID.Scan(stringValue(comment["source_task_id"]))
	out.authorType = stringValue(comment["author_type"])
	out.commentType = stringValue(comment["type"])
	out.content = stringValue(comment["content"])
	return out, out.id.Valid && out.content != ""
}

func stringValue(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case *string:
		if value != nil {
			return *value
		}
	}
	return ""
}

func (p *Patcher) sendDeliveryMessage(ctx context.Context, dq deliveryQueries, delivery db.ChannelDelivery, commentID pgtype.UUID, key, content, footer string) error {
	message, err := dq.CreateChannelDeliveryMessage(ctx, db.CreateChannelDeliveryMessageParams{
		DeliveryID: delivery.ID, SourceCommentID: commentID, IdempotencyKey: key,
	})
	if err != nil {
		return fmt.Errorf("create delivery message: %w", err)
	}
	if message.Status == "sent" {
		return nil
	}
	inst, err := p.queries.GetLarkInstallation(ctx, delivery.InstallationID)
	if err != nil {
		return fmt.Errorf("load delivery installation: %w", err)
	}
	if InstallationStatus(inst.Status) != InstallationActive {
		return nil
	}
	if footer != "" {
		content = appendMessageFooter(content, footer)
	}
	creds, err := p.installationCredentials(inst)
	if err != nil {
		return err
	}
	target := ReplyTarget{}
	if delivery.DestinationMessageID.Valid && delivery.DestinationThreadID.Valid {
		target = ReplyTarget{MessageID: delivery.DestinationMessageID.String, InThread: true}
	}
	var sentID string
	send := func(t ReplyTarget) error {
		if containsMarkdown(content) {
			sentID, err = p.client.SendMarkdownCard(ctx, SendMarkdownCardParams{InstallationID: creds, ChatID: ChatID(delivery.DestinationChatID.String), Markdown: content, ReplyTarget: t})
		} else {
			sentID, err = p.client.SendTextMessage(ctx, SendTextParams{InstallationID: creds, ChatID: ChatID(delivery.DestinationChatID.String), Text: content, ReplyTarget: t})
		}
		return err
	}
	if err := sendWithThreadFallback(p.cfg.Logger, "send routed delivery", target, send); err != nil {
		_, _ = dq.MarkChannelDeliveryMessageFailed(ctx, db.MarkChannelDeliveryMessageFailedParams{ID: message.ID, LastError: textOrNull("send_failed")})
		return err
	}
	if sentID == "" {
		return errors.New("lark delivery send returned an empty message id")
	}
	if _, err := dq.MarkChannelDeliveryMessageSent(ctx, db.MarkChannelDeliveryMessageSentParams{ID: message.ID, ChannelMessageID: textOrNull(sentID)}); err != nil {
		return fmt.Errorf("persist delivery message id: %w", err)
	}
	return nil
}

func (p *Patcher) deliveryFooter(ctx context.Context, delivery db.ChannelDelivery) string {
	agentName := ""
	if delivery.AgentID.Valid {
		if agent, err := p.queries.GetAgent(ctx, delivery.AgentID); err == nil {
			agentName = agent.Name
		}
	}
	issueIdentifier := ""
	if delivery.IssueID.Valid {
		if q, ok := p.queries.(issueFooterQueries); ok {
			if issue, err := q.GetIssue(ctx, delivery.IssueID); err == nil && issue.WorkspaceID == delivery.WorkspaceID {
				issueIdentifier = "#" + strconv.Itoa(int(issue.Number))
				if workspace, err := q.GetWorkspace(ctx, delivery.WorkspaceID); err == nil {
					if prefix := strings.TrimSpace(workspace.IssuePrefix); prefix != "" {
						issueIdentifier = prefix + "-" + strconv.Itoa(int(issue.Number))
					}
				}
			}
		}
	}
	return proactiveMessageFooter(agentName, issueIdentifier)
}

// sendChatReply turns ChatDonePayload.Content into a Lark message.
// The wire shape is chosen per-reply based on whether the body
// contains any markdown syntax:
//
//   - Plain prose (no markdown) → `msg_type=text`. A one-line "Hi!"
//     reply should feel like a normal IM message, not a notification
//     card with chrome around it.
//
//   - Anything with markdown (headings, lists, code blocks, tables,
//     bold/italic, links) → schema-2.0 interactive card with a
//     `tag: "markdown"` body element so Lark's client renders the
//     formatting instead of leaving raw `**bold**` characters in
//     the transcript. The card is visually subtler than the legacy
//     binding-prompt template — just a single markdown block, no
//     header / icon / CTA buttons.
//
// Empty content is silently dropped: we'd rather show nothing than
// "Done." (the prior card fallback that confused Bohan in the live
// dev env). In practice an empty Content means the daemon completed
// the task without producing visible output, which only happens for
// edge cases like a chat task that just acknowledged a system event;
// not emitting a message there is the right product call.
func (p *Patcher) sendChatReply(ctx context.Context, creds InstallationCredentials, binding ChatSessionBinding, payload any, footer string) error {
	content := chatDoneContent(payload)
	if content == "" {
		return nil
	}
	content = appendMessageFooter(content, footer)
	target := threadReplyTarget(binding)
	if containsMarkdown(content) {
		return sendWithThreadFallback(p.cfg.Logger, "send markdown card", target, func(t ReplyTarget) error {
			_, err := p.client.SendMarkdownCard(ctx, SendMarkdownCardParams{
				InstallationID: creds,
				ChatID:         outboundChatID(binding),
				Markdown:       content,
				ReplyTarget:    t,
			})
			return err
		})
	}
	return sendWithThreadFallback(p.cfg.Logger, "send text message", target, func(t ReplyTarget) error {
		_, err := p.client.SendTextMessage(ctx, SendTextParams{
			InstallationID: creds,
			ChatID:         outboundChatID(binding),
			Text:           content,
			ReplyTarget:    t,
		})
		return err
	})
}

// outboundChatID recovers the real Lark chat id from the chat binding. The
// channel_chat_id may be a composite "chat:thread" topic-isolation key, so
// the real chat id is read from the binding config (larkBindingConfig);
// pre-topic rows (config "{}") route by the key itself, which for them IS the
// real chat id.
func outboundChatID(b ChatSessionBinding) ChatID {
	if len(b.Config) > 0 {
		var cfg larkBindingConfig
		if err := json.Unmarshal(b.Config, &cfg); err == nil && cfg.ChatID != "" {
			return ChatID(cfg.ChatID)
		}
	}
	return ChatID(b.ChannelChatID)
}

// threadReplyTarget derives the outbound reply target from the chat
// binding's most-recent inbound trigger. We thread the reply ONLY when
// that trigger was itself inside a Lark topic (last_lark_thread_id
// present): normal group / p2p chats keep the unchanged chat-level send
// path, and only an @-mention that happened inside a thread gets a
// threaded reply (replying to last_lark_message_id with reply_in_thread).
// The zero ReplyTarget means "send at the chat level".
func threadReplyTarget(binding ChatSessionBinding) ReplyTarget {
	if binding.LastThreadID.Valid && binding.LastThreadID.String != "" &&
		binding.LastMessageID.Valid && binding.LastMessageID.String != "" {
		return ReplyTarget{MessageID: binding.LastMessageID.String, InThread: true}
	}
	return ReplyTarget{}
}

// sendWithThreadFallback runs send with the thread reply target and,
// ONLY when the threaded attempt fails with a Lark error that means the
// topic reply legitimately cannot land (trigger message recalled, topic
// gone, topics disabled, aggregated message — see
// threadReplyUnsupportedCodes), retries once at the chat level so the
// reply is not silently lost. Any other failure — transport error,
// 5xx, timeout, rate limit, or an ambiguous "the server may have
// received it" error — is logged and returned as a failure rather than
// retried: a blind chat-level retry could duplicate the reply or leak a
// thread-only reply into the main group chat. When target is already
// chat-level there is nothing to fall back to and the error is returned.
//
// It is a package-level function (rather than a Patcher method) so the
// event-driven Patcher and the immediate OutcomeReplier share one
// classified fallback path.
func sendWithThreadFallback(log *slog.Logger, op string, target ReplyTarget, send func(ReplyTarget) error) error {
	err := send(target)
	if err == nil {
		return nil
	}
	if target.IsSet() && isThreadReplyUnsupported(err) {
		log.Warn("lark: thread reply unsupported for target, retrying at chat level",
			"op", op, "reply_message_id", target.MessageID, "error", err)
		if fallbackErr := send(ReplyTarget{}); fallbackErr != nil {
			return fmt.Errorf("%s (chat-level fallback after thread-unsupported reply: %v): %w", op, err, fallbackErr)
		}
		return nil
	}
	if target.IsSet() {
		log.Warn("lark: thread reply failed; not falling back (non-classified error)",
			"op", op, "reply_message_id", target.MessageID, "error", err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func (p *Patcher) installationCredentials(inst Installation) (InstallationCredentials, error) {
	if p.credentials == nil {
		return InstallationCredentials{}, errors.New("lark patcher: credentials resolver missing")
	}
	secret, err := p.credentials.DecryptAppSecret(inst)
	if err != nil {
		return InstallationCredentials{}, fmt.Errorf("decrypt app_secret: %w", err)
	}
	creds := InstallationCredentials{
		AppID:     inst.AppID,
		AppSecret: secret,
		Region:    RegionOrDefault(inst.Region),
	}
	if inst.TenantKey.Valid {
		creds.TenantKey = inst.TenantKey.String
	}
	return creds, nil
}

// fail surfaces a short error card on task failure. Unlike the
// success path (plain text via sendChatReply), failures stay as cards
// because the user benefits from the visual distinction — a red /
// header-styled card is much harder to miss than a regular bubble,
// and these are rare enough that the card chrome isn't noisy.
//
// One-shot send (no patching, no DB row): if the task fails a second
// time we'd just send a second card, which is fine — failure is
// usually a single terminal event.
func (p *Patcher) fail(ctx context.Context, creds InstallationCredentials, binding ChatSessionBinding, taskID pgtype.UUID, agentName string, payload any) error {
	render, err := p.cfg.Renderer.Render(RenderInput{
		Kind:         CardKindError,
		AgentName:    agentName,
		TaskID:       taskID,
		ErrorMessage: errorMessageFromPayload(payload),
	})
	if err != nil {
		return fmt.Errorf("render error card: %w", err)
	}
	return sendWithThreadFallback(p.cfg.Logger, "send error card", threadReplyTarget(binding), func(t ReplyTarget) error {
		_, err := p.client.SendInteractiveCard(ctx, SendCardParams{
			InstallationID: creds,
			ChatID:         outboundChatID(binding),
			CardJSON:       render.JSON,
			ReplyTarget:    t,
		})
		return err
	})
}

// taskAndSessionFromEvent parses the typed-ish payload broadcastTaskEvent
// publishes — a map[string]any with `task_id` (always) and
// `chat_session_id` (chat tasks only). EventChatDone carries a
// ChatDonePayload struct instead.
func taskAndSessionFromEvent(e events.Event) (taskID, chatSessionID pgtype.UUID, ok bool) {
	if e.TaskID != "" {
		if err := taskID.Scan(e.TaskID); err != nil {
			taskID = pgtype.UUID{}
		}
	}
	if e.ChatSessionID != "" {
		if err := chatSessionID.Scan(e.ChatSessionID); err != nil {
			chatSessionID = pgtype.UUID{}
		}
	}
	switch p := e.Payload.(type) {
	case map[string]any:
		if !taskID.Valid {
			if s, _ := p["task_id"].(string); s != "" {
				_ = taskID.Scan(s)
			}
		}
		if !chatSessionID.Valid {
			if s, _ := p["chat_session_id"].(string); s != "" {
				_ = chatSessionID.Scan(s)
			}
		}
	case protocol.ChatDonePayload:
		if !taskID.Valid {
			_ = taskID.Scan(p.TaskID)
		}
		if !chatSessionID.Valid {
			_ = chatSessionID.Scan(p.ChatSessionID)
		}
	}
	return taskID, chatSessionID, taskID.Valid
}

func chatDoneContent(payload any) string {
	switch p := payload.(type) {
	case protocol.ChatDonePayload:
		return p.Content
	case map[string]any:
		if s, ok := p["content"].(string); ok {
			return s
		}
	}
	return ""
}

func errorMessageFromPayload(payload any) string {
	if m, ok := payload.(map[string]any); ok {
		if s, ok := m["error"].(string); ok {
			return s
		}
		if s, ok := m["error_message"].(string); ok {
			return s
		}
	}
	return ""
}
