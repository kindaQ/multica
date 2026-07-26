package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var supportedFeishuNotificationEvents = map[string]struct{}{
	"issue.done":         {},
	"issue.blocked":      {},
	"dispatch.completed": {},
	"dispatch.failed":    {},
}

var defaultFeishuNotificationEvents = []string{
	"issue.done",
	"issue.blocked",
	"dispatch.completed",
	"dispatch.failed",
}

type WorkspaceChannelSettingResponse struct {
	WorkspaceID                 string   `json:"workspace_id"`
	ChannelType                 string   `json:"channel_type"`
	DefaultAgentID              *string  `json:"default_agent_id"`
	NotificationRecipientUserID *string  `json:"notification_recipient_user_id"`
	NotificationEnabled         bool     `json:"notification_enabled"`
	NotificationEvents          []string `json:"notification_events"`
}

type UpdateWorkspaceChannelSettingRequest struct {
	DefaultAgentID              *string   `json:"default_agent_id"`
	NotificationRecipientUserID *string   `json:"notification_recipient_user_id"`
	NotificationEnabled         *bool     `json:"notification_enabled"`
	NotificationEvents          *[]string `json:"notification_events"`
}

func workspaceChannelSettingResponse(workspaceID pgtype.UUID, row *db.WorkspaceChannelSetting) WorkspaceChannelSettingResponse {
	resp := WorkspaceChannelSettingResponse{
		WorkspaceID:        uuidToString(workspaceID),
		ChannelType:        "feishu",
		NotificationEvents: append([]string(nil), defaultFeishuNotificationEvents...),
	}
	if row == nil {
		return resp
	}
	resp.DefaultAgentID = optionalUUIDString(row.DefaultAgentID)
	resp.NotificationRecipientUserID = optionalUUIDString(row.NotificationRecipientUserID)
	resp.NotificationEnabled = row.NotificationEnabled
	if err := json.Unmarshal(row.NotificationEvents, &resp.NotificationEvents); err != nil {
		resp.NotificationEvents = append([]string(nil), defaultFeishuNotificationEvents...)
	}
	return resp
}

func (h *Handler) GetFeishuWorkspaceSetting(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	row, err := h.Queries.GetWorkspaceChannelSetting(r.Context(), db.GetWorkspaceChannelSettingParams{
		WorkspaceID: workspaceID,
		ChannelType: "feishu",
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, workspaceChannelSettingResponse(workspaceID, nil))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load Feishu workspace settings")
		return
	}
	writeJSON(w, http.StatusOK, workspaceChannelSettingResponse(workspaceID, &row))
}

func (h *Handler) UpdateFeishuWorkspaceSetting(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	var req UpdateWorkspaceChannelSettingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	current, err := h.Queries.GetWorkspaceChannelSetting(r.Context(), db.GetWorkspaceChannelSettingParams{
		WorkspaceID: workspaceID,
		ChannelType: "feishu",
	})
	if errors.Is(err, pgx.ErrNoRows) {
		current = db.WorkspaceChannelSetting{
			WorkspaceID:        workspaceID,
			ChannelType:        "feishu",
			NotificationEvents: mustJSON(defaultFeishuNotificationEvents),
		}
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load Feishu workspace settings")
		return
	}

	defaultAgentID := current.DefaultAgentID
	if req.DefaultAgentID != nil {
		if *req.DefaultAgentID == "" {
			defaultAgentID = pgtype.UUID{}
		} else {
			parsed, ok := parseUUIDOrBadRequest(w, *req.DefaultAgentID, "default_agent_id")
			if !ok {
				return
			}
			agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
				ID:          parsed,
				WorkspaceID: workspaceID,
			})
			if err != nil || agent.ArchivedAt.Valid || !h.agentInvocableByWorkspace(r.Context(), agent) {
				writeError(w, http.StatusBadRequest, "default agent must be an active workspace-invocable agent")
				return
			}
			defaultAgentID = parsed
		}
	}

	recipientID := current.NotificationRecipientUserID
	if req.NotificationRecipientUserID != nil {
		if *req.NotificationRecipientUserID == "" {
			recipientID = pgtype.UUID{}
		} else {
			parsed, ok := parseUUIDOrBadRequest(w, *req.NotificationRecipientUserID, "notification_recipient_user_id")
			if !ok {
				return
			}
			if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
				UserID:      parsed,
				WorkspaceID: workspaceID,
			}); err != nil {
				writeError(w, http.StatusBadRequest, "notification recipient must be a workspace member")
				return
			}
			state, err := h.Queries.GetInstanceState(r.Context())
			if err != nil || !state.PublicChannelInstallationID.Valid {
				writeError(w, http.StatusConflict, "the public Feishu bot is not ready")
				return
			}
			if _, err := h.Queries.GetChannelAccountBindingByMulticaUser(r.Context(), db.GetChannelAccountBindingByMulticaUserParams{
				InstallationID: state.PublicChannelInstallationID,
				MulticaUserID:  parsed,
			}); err != nil {
				writeError(w, http.StatusBadRequest, "notification recipient has not bound a Feishu account")
				return
			}
			recipientID = parsed
		}
	}

	enabled := current.NotificationEnabled
	if req.NotificationEnabled != nil {
		enabled = *req.NotificationEnabled
	}
	if enabled && !recipientID.Valid {
		writeError(w, http.StatusBadRequest, "notification recipient is required when Feishu notifications are enabled")
		return
	}

	events := append([]byte(nil), current.NotificationEvents...)
	if req.NotificationEvents != nil {
		normalized := make([]string, 0, len(*req.NotificationEvents))
		seen := map[string]struct{}{}
		for _, eventType := range *req.NotificationEvents {
			if _, ok := supportedFeishuNotificationEvents[eventType]; !ok {
				writeError(w, http.StatusBadRequest, "unsupported Feishu notification event")
				return
			}
			if _, ok := seen[eventType]; ok {
				continue
			}
			seen[eventType] = struct{}{}
			normalized = append(normalized, eventType)
		}
		events = mustJSON(normalized)
	}

	updated, err := h.Queries.UpsertWorkspaceChannelSetting(r.Context(), db.UpsertWorkspaceChannelSettingParams{
		WorkspaceID:                 workspaceID,
		ChannelType:                 "feishu",
		DefaultAgentID:              defaultAgentID,
		NotificationRecipientUserID: recipientID,
		NotificationEnabled:         enabled,
		NotificationEvents:          events,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update Feishu workspace settings")
		return
	}
	writeJSON(w, http.StatusOK, workspaceChannelSettingResponse(workspaceID, &updated))
}

func (h *Handler) agentInvocableByWorkspace(ctx context.Context, agent db.Agent) bool {
	if agent.PermissionMode != "public_to" {
		return false
	}
	targets, err := h.Queries.ListAgentInvocationTargets(ctx, agent.ID)
	if err != nil {
		return false
	}
	for _, target := range targets {
		if target.TargetType == "workspace" && target.TargetID == agent.WorkspaceID {
			return true
		}
	}
	return false
}

func mustJSON(value any) []byte {
	out, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return out
}
