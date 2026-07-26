package lark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	notificationWorkerConcurrency = 2
	notificationMaxAttempts       = 8
)

type issueNotificationPayload struct {
	Issue struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
		Identifier  string `json:"identifier"`
		Title       string `json:"title"`
		Status      string `json:"status"`
		UpdatedAt   string `json:"updated_at"`
	} `json:"issue"`
	StatusChanged bool `json:"status_changed"`
}

// NotificationService turns terminal issue status events into a durable
// Postgres outbox and delivers each row through the singleton public bot.
// Event subscription is only an ingress hint; retry, leasing and idempotency
// remain durable across process restarts and multiple server replicas.
type NotificationService struct {
	queries     *db.Queries
	store       *ChannelStore
	client      APIClient
	credentials CredentialsResolver
	logger      *slog.Logger
	notify      chan struct{}
}

func NewNotificationService(queries *db.Queries, client APIClient, credentials CredentialsResolver, bus *events.Bus, logger *slog.Logger) *NotificationService {
	if logger == nil {
		logger = slog.Default()
	}
	service := &NotificationService{
		queries:     queries,
		store:       NewChannelStore(queries),
		client:      client,
		credentials: credentials,
		logger:      logger,
		notify:      make(chan struct{}, notificationWorkerConcurrency),
	}
	if bus != nil {
		// The database trigger has already inserted the outbox row before the
		// issue:updated event is published. This listener is only a low-latency
		// wake hint; the one-second sweep is the durable recovery path.
		bus.Subscribe(protocol.EventIssueUpdated, func(events.Event) {
			service.Notify()
		})
	}
	return service
}

func notificationEventEnabled(raw []byte, eventType string) bool {
	var values []string
	if json.Unmarshal(raw, &values) != nil {
		return false
	}
	for _, value := range values {
		if value == eventType {
			return true
		}
	}
	return false
}

func (s *NotificationService) Notify() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *NotificationService) Run(ctx context.Context) {
	if s == nil || s.queries == nil || s.client == nil || s.credentials == nil {
		return
	}
	var workers sync.WaitGroup
	workers.Add(notificationWorkerConcurrency)
	for range notificationWorkerConcurrency {
		go func() {
			defer workers.Done()
			s.runLoop(ctx)
		}()
	}
	workers.Wait()
}

func (s *NotificationService) runLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		worked, err := s.ProcessNext(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Error("lark notification: process delivery", "error", err)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.notify:
		case <-ticker.C:
		}
	}
}

func (s *NotificationService) ProcessNext(ctx context.Context) (bool, error) {
	delivery, err := s.queries.ClaimChannelNotificationDelivery(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim delivery: %w", err)
	}
	cancel := func(code string) (bool, error) {
		_, cancelErr := s.queries.CancelChannelNotificationDelivery(ctx, db.CancelChannelNotificationDeliveryParams{
			LastErrorCode: pgtype.Text{String: code, Valid: true},
			ID:            delivery.ID,
			LeaseToken:    delivery.LeaseToken,
		})
		return true, cancelErr
	}

	instance, err := s.queries.GetInstanceState(ctx)
	if err != nil || instance.Status != "ready" || !instance.PublicChannelInstallationID.Valid ||
		instance.PublicChannelInstallationID != delivery.InstallationID {
		return cancel("public_gateway_unavailable")
	}
	setting, err := s.queries.GetWorkspaceChannelSetting(ctx, db.GetWorkspaceChannelSettingParams{
		WorkspaceID: delivery.WorkspaceID,
		ChannelType: channelTypeFeishu,
	})
	if err != nil || !setting.NotificationEnabled || setting.NotificationRecipientUserID != delivery.RecipientUserID ||
		!notificationEventEnabled(setting.NotificationEvents, delivery.EventType) {
		return cancel("notification_setting_changed")
	}
	binding, err := s.queries.GetChannelAccountBindingByMulticaUser(ctx, db.GetChannelAccountBindingByMulticaUserParams{
		InstallationID: delivery.InstallationID,
		MulticaUserID:  delivery.RecipientUserID,
	})
	if err != nil || binding.ChannelUserID != delivery.RecipientChannelUserID {
		return cancel("recipient_binding_changed")
	}
	inst, err := s.store.GetLarkInstallation(ctx, delivery.InstallationID)
	if err != nil || inst.Status != string(InstallationActive) {
		return cancel("public_installation_inactive")
	}
	secret, err := s.credentials.DecryptAppSecret(inst)
	if err != nil {
		return s.retry(ctx, delivery, "credential_error")
	}
	creds := InstallationCredentials{
		AppID:     inst.AppID,
		AppSecret: secret,
		Region:    RegionOrDefault(inst.Region),
	}
	if inst.TenantKey.Valid {
		creds.TenantKey = inst.TenantKey.String
	}
	text, err := renderIssueNotification(delivery.EventType, delivery.Payload)
	if err != nil {
		return cancel("invalid_payload")
	}
	messageID, err := s.client.SendTextMessage(ctx, SendTextParams{
		InstallationID: creds,
		OpenID:         OpenID(delivery.RecipientChannelUserID),
		Text:           text,
	})
	if err != nil {
		return s.retry(ctx, delivery, classifyNotificationError(err))
	}
	_, err = s.queries.MarkChannelNotificationDeliverySent(ctx, db.MarkChannelNotificationDeliverySentParams{
		ChannelMessageID: pgtype.Text{String: messageID, Valid: messageID != ""},
		ID:               delivery.ID,
		LeaseToken:       delivery.LeaseToken,
	})
	return true, err
}

func renderIssueNotification(eventType string, raw []byte) (string, error) {
	var payload issueNotificationPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	identifier := strings.TrimSpace(payload.Issue.Identifier)
	if identifier == "" {
		identifier = "Issue"
	}
	switch eventType {
	case "issue.done":
		return fmt.Sprintf("✅ %s 已完成\n%s", identifier, payload.Issue.Title), nil
	case "issue.blocked":
		return fmt.Sprintf("⛔ %s 已阻塞\n%s", identifier, payload.Issue.Title), nil
	default:
		return "", errors.New("unsupported notification event")
	}
}

func (s *NotificationService) retry(ctx context.Context, delivery db.ChannelNotificationDelivery, code string) (bool, error) {
	delaySeconds := math.Pow(2, float64(max(delivery.AttemptCount-1, 0)))
	delay := time.Duration(delaySeconds) * time.Second
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	_, err := s.queries.RetryChannelNotificationDelivery(ctx, db.RetryChannelNotificationDeliveryParams{
		MaxAttempts:   notificationMaxAttempts,
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now().Add(delay), Valid: true},
		LastErrorCode: pgtype.Text{String: code, Valid: true},
		ID:            delivery.ID,
		LeaseToken:    delivery.LeaseToken,
	})
	return true, err
}

func classifyNotificationError(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("lark_api_%d", apiErr.Code)
	}
	return "transport_error"
}
